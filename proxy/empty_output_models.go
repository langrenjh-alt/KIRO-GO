package proxy

import (
	"strings"
	"time"

	"kiro-go/config"
	"kiro-go/pool"
)

// emptyOutputModelView is one model the account cannot serve because of an
// events=none cooldown. Until is unix seconds.
type emptyOutputModelView struct {
	Model string `json:"model"`
	Until int64  `json:"until"`
}

// emptyOutputModelViews groups active cooldowns by account. The switch gates
// the list the same way it gates scheduling: a disabled switch is not a limit.
func emptyOutputModelViews(items []pool.ModelCooldown) map[string][]emptyOutputModelView {
	if !config.EmptyOutputModelCooldownEnabled() {
		return nil
	}
	out := make(map[string][]emptyOutputModelView)
	for _, item := range items {
		model := strings.TrimSpace(item.Model)
		if item.AccountID == "" || model == "" {
			continue
		}
		out[item.AccountID] = append(out[item.AccountID], emptyOutputModelView{
			Model: model,
			Until: item.Until.Unix(),
		})
	}
	return out
}

func emptyOutputModelsForAccount(views map[string][]emptyOutputModelView, accountID string) []emptyOutputModelView {
	if models := views[accountID]; len(models) > 0 {
		return models
	}
	return []emptyOutputModelView{}
}

func (h *Handler) emptyOutputModelViewsNow(now time.Time) map[string][]emptyOutputModelView {
	if h == nil || h.pool == nil {
		return nil
	}
	return emptyOutputModelViews(h.pool.ListModelCooldowns(now))
}
