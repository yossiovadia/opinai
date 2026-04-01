package dashboard

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/yossiovadia/opinai/controller-go/internal/database"
)

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	uptime := time.Since(s.state.StartTime).Seconds()
	dbStats, _ := database.GetTotalStats()

	json.NewEncoder(w).Encode(map[string]any{
		"uptime_seconds":  int(uptime),
		"uptime_human":    FormatDuration(uptime),
		"last_poll":       s.state.GetLastPoll(),
		"poll_count":      s.state.GetPollCount(),
		"repos_count":     len(s.state.GetRepos()),
		"total_runs":      dbStats.TotalRuns,
		"total_processed": dbStats.TotalProcessed,
	})
}

func (s *Server) handleRepos(w http.ResponseWriter, r *http.Request) {
	repos := s.state.GetRepos()
	result := make([]map[string]any, 0, len(repos))
	for name, status := range repos {
		result = append(result, map[string]any{
			"name":        name,
			"pending":     status.Pending,
			"processed":   status.Processed,
			"manual_only": status.ManualOnly,
			"last_check":  status.LastCheck,
		})
	}
	json.NewEncoder(w).Encode(result)
}

func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	limitStr := r.URL.Query().Get("limit")
	limit := 50
	if n, err := strconv.Atoi(limitStr); err == nil && n > 0 {
		limit = n
	}
	repo := r.URL.Query().Get("repo")

	runs, err := database.GetRuns(repo, limit)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
		return
	}
	if runs == nil {
		runs = []database.Run{}
	}
	json.NewEncoder(w).Encode(runs)
}
