package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aarani/craftling-go/internal/model"
	"github.com/aarani/craftling-go/internal/registry"
	"github.com/gin-gonic/gin"
)

// fakeResolver serves a fixed manifest for one id and ErrNotFound otherwise.
type fakeResolver struct{ m *registry.Manifest }

func (f fakeResolver) ManifestParsed(_ context.Context, id string) (*registry.Manifest, error) {
	if id == "vanilla" {
		return f.m, nil
	}
	return nil, registry.ErrNotFound
}

func testManifest() *registry.Manifest {
	return &registry.Manifest{
		ImageName:  "itzg/minecraft-server",
		ImageTag:   "java21",
		EULANeeded: true,
		Variables: []registry.Variable{
			{Name: "DIFFICULTY", AcceptableAnswers: []string{"peaceful", "hard"}},
		},
		Env: map[string]string{"EULA": "TRUE", "DIFFICULTY": "$DIFFICULTY$"},
	}
}

func newTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/servers", nil)
	return c, w
}

func TestApplyTemplateSuccess(t *testing.T) {
	h := &ServerHandler{templates: fakeResolver{m: testManifest()}}
	c, w := newTestContext()
	s := &model.GameServer{}
	req := &createServerRequest{
		Name: "survival", TemplateID: "vanilla", EULAAccepted: true,
		Answers: map[string]string{"DIFFICULTY": "hard"},
	}

	if !h.applyTemplate(c, req, s) {
		t.Fatalf("applyTemplate returned false (status %d)", w.Code)
	}
	if s.ImageRef == nil || *s.ImageRef != "itzg/minecraft-server:java21" {
		t.Errorf("ImageRef = %v, want itzg/minecraft-server:java21", s.ImageRef)
	}
	if s.TemplateID == nil || *s.TemplateID != "vanilla" {
		t.Errorf("TemplateID = %v, want vanilla", s.TemplateID)
	}
	if s.Version != "java21" {
		t.Errorf("Version = %q, want java21", s.Version)
	}
	if s.Env["DIFFICULTY"] != "hard" || s.Env["EULA"] != "TRUE" {
		t.Errorf("Env = %v", s.Env)
	}
}

func TestApplyTemplateErrors(t *testing.T) {
	cases := []struct {
		name string
		req  *createServerRequest
		code int
	}{
		{
			name: "unknown template",
			req:  &createServerRequest{Name: "x", TemplateID: "missing", EULAAccepted: true},
			code: http.StatusNotFound,
		},
		{
			name: "missing eula",
			req:  &createServerRequest{Name: "x", TemplateID: "vanilla", Answers: map[string]string{"DIFFICULTY": "hard"}},
			code: http.StatusBadRequest,
		},
		{
			name: "invalid answer",
			req:  &createServerRequest{Name: "x", TemplateID: "vanilla", EULAAccepted: true, Answers: map[string]string{"DIFFICULTY": "nightmare"}},
			code: http.StatusBadRequest,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &ServerHandler{templates: fakeResolver{m: testManifest()}}
			c, w := newTestContext()
			if h.applyTemplate(c, tc.req, &model.GameServer{}) {
				t.Fatalf("applyTemplate returned true, want false")
			}
			if w.Code != tc.code {
				t.Errorf("status = %d, want %d", w.Code, tc.code)
			}
		})
	}
}
