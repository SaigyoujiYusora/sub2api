package handler

import (
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func executePortableCompactionPlan(c *gin.Context, plan *service.PortableCompactionPlan, key *service.APIKey, body []byte) ([]byte, bool, error) {
	if key.Group != nil && key.Group.ModelAllowlistEnabled() && !key.Group.ModelAllowlist.Allows(plan.SummaryModel()) {
		return nil, false, fmt.Errorf("plaintext summary model %q is not available for this group", plan.SummaryModel())
	}
	scope := service.PortableSummaryScope{UserID: key.UserID, APIKeyID: key.ID}
	if key.GroupID != nil {
		scope.GroupID = *key.GroupID
	}
	return plan.Execute(c, scope, body)
}
