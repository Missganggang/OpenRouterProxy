package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/openroute/openroute/internal/model"
)

func TestReportOnlyCurrentAcceptedRecoveryClearsNodeError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name         string
		version      int64
		results      []RuleSyncResult
		otherFailure bool
		editedRule   bool
		wantCleared  bool
	}{
		{name: "old traffic batch", version: 0},
		{name: "current traffic without sync results", version: 1},
		{name: "old success", version: 0, results: []RuleSyncResult{{RuleID: 1, Status: "normal"}}},
		{name: "unknown rule success", version: 1, results: []RuleSyncResult{{RuleID: 999, Status: "normal"}}},
		{name: "current failure", version: 1, results: []RuleSyncResult{{RuleID: 1, Status: "failed", Error: "port busy"}}},
		{name: "another rule still failed", version: 1, results: []RuleSyncResult{{RuleID: 1, Status: "normal"}}, otherFailure: true},
		{name: "edited rule with old success receipt", version: 1, results: []RuleSyncResult{{RuleID: 1, Status: "normal"}}, editedRule: true},
		{name: "current accepted success", version: 1, results: []RuleSyncResult{{RuleID: 1, Status: "normal"}}, wantCleared: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, node := newConfigSnapshotFixture(t)
			if _, err := NewConfigBuilder(a).BuildFull(context.Background(), node); err != nil {
				t.Fatal(err)
			}
			const message = "current configuration failed"
			if err := a.DB.Model(node).Update("last_error", message).Error; err != nil {
				t.Fatal(err)
			}
			if tc.otherFailure {
				if err := a.DB.Create(&model.NodeRuleSync{NodeID: node.ID, RuleID: 2, ConfigVersion: 1, Status: model.SyncFailed}).Error; err != nil {
					t.Fatal(err)
				}
			}
			if tc.editedRule {
				if err := a.DB.Create(&model.NodeRuleSync{NodeID: node.ID, RuleID: 1, ConfigVersion: 1, Status: model.SyncNormal}).Error; err != nil {
					t.Fatal(err)
				}
				if err := a.DB.Model(&model.ForwardRule{}).Where("id = 1").Updates(map[string]interface{}{"listen_port": 34567, "sync_status": model.SyncUnsynced}).Error; err != nil {
					t.Fatal(err)
				}
			}
			body, err := json.Marshal(ReportRequest{NodeID: node.ID, BatchID: "test-report", ConfigVersion: tc.version, Results: tc.results})
			if err != nil {
				t.Fatal(err)
			}
			router := gin.New()
			router.POST("/api/node/report", NewRegistry(a).ReportHandler)
			request := httptest.NewRequest(http.MethodPost, "/api/node/report", bytes.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-Node-Token", node.Token)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			var saved model.Node
			if err := a.DB.First(&saved, node.ID).Error; err != nil {
				t.Fatal(err)
			}
			if (saved.LastError == "") != tc.wantCleared {
				t.Fatalf("last_error=%q clear=%v", saved.LastError, tc.wantCleared)
			}
		})
	}
}
