package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/openroute/openroute/internal/model"
)

func TestManagementRoleAndTokenScopes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name, role, scope string
		token             *model.APIToken
		want              int
	}{
		{"user cannot exec", "user", ScopeNodeExec, nil, 403},
		{"user cannot manage nodes", "user", ScopeNodeWrite, nil, 403},
		{"user cannot change groups", "user", ScopeGroupWrite, nil, 403},
		{"user may read own traffic", "user", ScopeTrafficRead, nil, 204},
		{"admin may exec", "admin", ScopeNodeExec, nil, 204},
		{"token still needs scope", "admin", ScopeNodeExec, &model.APIToken{Scopes: model.FromAny([]string{ScopeNodeRead})}, 403},
		{"scoped token may exec", "admin", ScopeNodeExec, &model.APIToken{Scopes: model.FromAny([]string{ScopeNodeExec})}, 204},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := gin.New()
			r.GET("/", func(c *gin.Context) {
				c.Set(CtxUserID, uint64(1))
				c.Set(CtxRole, test.role)
				if test.token != nil {
					c.Set(CtxAPIToken, test.token)
				}
			}, RequireScope(test.scope), func(c *gin.Context) { c.Status(204) })
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
			if w.Code != test.want {
				t.Fatalf("status=%d want=%d body=%s", w.Code, test.want, w.Body.String())
			}
		})
	}
}
