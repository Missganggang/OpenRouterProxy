package api

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/openroute/openroute/internal/api/middleware"
)

func TestTrafficFilterCannotOverrideAuthenticatedOwner(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, role := range []string{"user", "admin"} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("GET", "/?user_id=99&rule_id=3", nil)
		c.Set(middleware.CtxUserID, uint64(7))
		c.Set(middleware.CtxRole, role)
		filter := trafficFilter(c)
		want := uint64(7)
		if role == "admin" {
			want = 99
		}
		if filter.UserID != want || filter.RuleID != 3 {
			t.Fatalf("%s filter: %+v", role, filter)
		}
	}
}
