package proxy

import (
	"encoding/json"
	"errors"
	"net/http"

	"kiro-go/config"
)

func (h *Handler) apiListPromptCachePolicies(w http.ResponseWriter, _ *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"policies": config.ListPromptCachePolicies(),
	})
}

func (h *Handler) apiGetPromptCachePolicy(w http.ResponseWriter, _ *http.Request, id string) {
	policy := config.GetPromptCachePolicy(id)
	if policy == nil {
		writePromptCachePolicyError(w, config.ErrPromptCachePolicyNotFound)
		return
	}
	json.NewEncoder(w).Encode(policy)
}

func (h *Handler) apiCreatePromptCachePolicy(w http.ResponseWriter, r *http.Request) {
	var policy config.PromptCachePolicy
	if err := json.NewDecoder(r.Body).Decode(&policy); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	policy.ID = ""
	policy.CreatedAt = 0
	policy.UpdatedAt = 0
	created, err := config.AddPromptCachePolicy(policy)
	if err != nil {
		writePromptCachePolicyError(w, err)
		return
	}
	// Build and load the runtime tracker on the admin path so the first request
	// bound to a newly created policy never pays disk I/O or heap rebuild cost.
	h.promptCacheTrackerForPolicy(created)
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "policy": created})
}

func (h *Handler) apiUpdatePromptCachePolicy(w http.ResponseWriter, r *http.Request, id string) {
	var policy config.PromptCachePolicy
	if err := json.NewDecoder(r.Body).Decode(&policy); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	updated, err := config.UpdatePromptCachePolicy(id, policy)
	if err != nil {
		writePromptCachePolicyError(w, err)
		return
	}
	h.applyPromptCachePolicy(updated)
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "policy": updated})
}

func (h *Handler) apiDeletePromptCachePolicy(w http.ResponseWriter, _ *http.Request, id string) {
	if err := config.DeletePromptCachePolicy(id); err != nil {
		writePromptCachePolicyError(w, err)
		return
	}
	h.removePromptCachePolicyTracker(id)
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func writePromptCachePolicyError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, config.ErrPromptCachePolicyNotFound):
		status = http.StatusNotFound
	case errors.Is(err, config.ErrPromptCachePolicyInUse):
		status = http.StatusConflict
	case errors.Is(err, config.ErrPromptCachePolicyInvalid):
		status = http.StatusBadRequest
	}
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
