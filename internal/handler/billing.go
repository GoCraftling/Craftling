package handler

import (
	"errors"
	"net/http"
	"time"

	"github.com/aarani/craftling-go/internal/billing"
	"github.com/aarani/craftling-go/internal/logger"
	"github.com/aarani/craftling-go/internal/middleware"
	"github.com/aarani/craftling-go/internal/model"
	"github.com/aarani/craftling-go/internal/repository"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// BillingHandler serves the pay-as-you-go billing endpoints (P9): a user's own
// bill and the admin view of any user's bill.
type BillingHandler struct {
	billing *repository.BillingRepository
	users   *repository.UserRepository
	rates   billing.Rates
}

// NewBillingHandler constructs a BillingHandler with the active price list.
func NewBillingHandler(bill *repository.BillingRepository, users *repository.UserRepository, rates billing.Rates) *BillingHandler {
	return &BillingHandler{billing: bill, users: users, rates: rates}
}

// Mine returns the authenticated caller's bill for the current period.
func (h *BillingHandler) Mine(c *gin.Context) {
	h.writeSummary(c, middleware.UserIDFromContext(c))
}

// GetForUser returns any user's bill. Guarded by RequireRole(admin). Responds
// 404 if the user does not exist.
func (h *BillingHandler) GetForUser(c *gin.Context) {
	id := c.Param("id")
	if _, err := h.users.GetByID(c.Request.Context(), id); err != nil {
		if !errors.Is(err, repository.ErrNotFound) {
			logger.FromContext(c).Error("get user", zap.Error(err))
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
			return
		}
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}
	h.writeSummary(c, id)
}

// writeSummary computes and writes a user's bill for the current period. The
// period is month-to-date in UTC — the natural pay-as-you-go cycle — with each
// running interval clamped to the period and open intervals measured to now.
func (h *BillingHandler) writeSummary(c *gin.Context, userID string) {
	periodStart := startOfMonthUTC(time.Now())
	rows, err := h.billing.OwnerLedger(c.Request.Context(), userID, periodStart)
	if err != nil {
		logger.FromContext(c).Error("owner ledger", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	summary := model.BillingSummary{
		Currency:     h.rates.Currency,
		PeriodStart:  periodStart,
		CPUHour:      h.rates.CPUHour,
		MemoryGBHour: h.rates.MemoryGBHour,
		Items:        make([]model.BillingLineItem, 0, len(rows)),
	}
	for _, row := range rows {
		hourly := billing.Round2(h.rates.HourlyRate(row.CPUs, row.MemoryMB))
		cost := billing.Round2(h.rates.CostSeconds(row.CPUs, row.MemoryMB, row.Seconds))
		summary.Items = append(summary.Items, model.BillingLineItem{
			ServerID:   row.ServerID,
			Name:       row.Name,
			CPUs:       row.CPUs,
			MemoryMB:   row.MemoryMB,
			Hours:      round2(row.Seconds / 3600.0),
			HourlyRate: hourly,
			Cost:       cost,
			Running:    row.Running,
		})
		summary.TotalCost += cost
		if row.Running {
			summary.HourlyRate += hourly
		}
	}
	summary.TotalCost = billing.Round2(summary.TotalCost)
	summary.HourlyRate = billing.Round2(summary.HourlyRate)
	c.JSON(http.StatusOK, summary)
}

// startOfMonthUTC returns midnight on the first of t's month, in UTC.
func startOfMonthUTC(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func round2(v float64) float64 { return billing.Round2(v) }
