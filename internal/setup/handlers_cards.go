package setup

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/fluctio-ai/fluctio/internal/kb"
)

// Cards API — the spaced-repetition Q&A flashcards behind
// /agents/{id}/knowledge/cards/. CRUD + archive/restore mirror the
// bookmark handlers; /review applies one Ebbinghaus grade; /stats feeds
// the page header (due today, status counts, streak).

// handleKBListCards pages the card library. Query params:
// filter=due|all|active|mastered|archived|new (default all), source=
// diary|wiki|manual, q=question/answer substring, limit/offset.
func (s *Server) handleKBListCards(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	if agentID == "" {
		http.Error(w, "missing agent id", http.StatusBadRequest)
		return
	}
	q := r.URL.Query()
	limit := 50
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	offset := 0
	if v := q.Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	kbStore := s.kbStoreFor(agentID)
	if kbStore == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	cards, err := kbStore.ListCards(r.Context(), agentID, q.Get("filter"), q.Get("source"), q.Get("q"), limit, offset)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if cards == nil {
		cards = []kb.KBCard{}
	}
	writeJSON(w, http.StatusOK, cards)
}

// handleKBGetCard returns one card plus its review timeline.
func (s *Server) handleKBGetCard(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	cardID := r.PathValue("cardId")
	if agentID == "" || cardID == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	kbStore := s.kbStoreFor(agentID)
	if kbStore == nil {
		http.Error(w, "knowledge base not available", http.StatusServiceUnavailable)
		return
	}
	card, err := kbStore.GetCard(r.Context(), agentID, cardID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	reviews, _ := kbStore.ListCardReviews(r.Context(), agentID, cardID)
	if reviews == nil {
		reviews = []kb.KBCardReview{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"card": card, "reviews": reviews})
}

// handleKBSaveCard hand-writes one card ("manual" source). Returns its id.
func (s *Server) handleKBSaveCard(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	if agentID == "" {
		http.Error(w, "missing agent id", http.StatusBadRequest)
		return
	}
	var req struct {
		Question string `json:"question"`
		Answer   string `json:"answer"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Question == "" {
		http.Error(w, "question is required", http.StatusBadRequest)
		return
	}
	kbStore := s.kbStoreFor(agentID)
	if kbStore == nil {
		http.Error(w, "knowledge base not available", http.StatusServiceUnavailable)
		return
	}
	id, err := kbStore.SaveCard(r.Context(), agentID, req.Question, req.Answer, "manual", "", "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

// handleKBUpdateCard edits question and/or answer without touching the
// review state.
func (s *Server) handleKBUpdateCard(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	cardID := r.PathValue("cardId")
	if agentID == "" || cardID == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	var req struct {
		Question string `json:"question,omitempty"`
		Answer   string `json:"answer,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	kbStore := s.kbStoreFor(agentID)
	if kbStore == nil {
		http.Error(w, "knowledge base not available", http.StatusServiceUnavailable)
		return
	}
	if err := kbStore.UpdateCard(r.Context(), agentID, cardID, req.Question, req.Answer); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// handleKBDeleteCard removes one card (plus reviews + embedding).
func (s *Server) handleKBDeleteCard(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	cardID := r.PathValue("cardId")
	if agentID == "" || cardID == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	kbStore := s.kbStoreFor(agentID)
	if kbStore == nil {
		http.Error(w, "knowledge base not available", http.StatusServiceUnavailable)
		return
	}
	if err := kbStore.DeleteCard(r.Context(), agentID, cardID); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handleKBReviewCard applies one grade (forgot|fuzzy|remembered) and
// returns the updated card so the client can refresh counts live.
func (s *Server) handleKBReviewCard(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	cardID := r.PathValue("cardId")
	if agentID == "" || cardID == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	var req struct {
		Grade string `json:"grade"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	kbStore := s.kbStoreFor(agentID)
	if kbStore == nil {
		http.Error(w, "knowledge base not available", http.StatusServiceUnavailable)
		return
	}
	card, err := kbStore.ReviewCard(r.Context(), agentID, cardID, req.Grade)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, card)
}

// handleKBCardArchive / handleKBCardRestore toggle a card's archived state.
func (s *Server) handleKBCardArchive(w http.ResponseWriter, r *http.Request) {
	s.handleKBCardSetStatus(w, r, "archived")
}

func (s *Server) handleKBCardRestore(w http.ResponseWriter, r *http.Request) {
	s.handleKBCardSetStatus(w, r, "active")
}

func (s *Server) handleKBCardSetStatus(w http.ResponseWriter, r *http.Request, status string) {
	agentID := r.PathValue("id")
	cardID := r.PathValue("cardId")
	if agentID == "" || cardID == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	kbStore := s.kbStoreFor(agentID)
	if kbStore == nil {
		http.Error(w, "knowledge base not available", http.StatusServiceUnavailable)
		return
	}
	var err error
	if status == "archived" {
		err = kbStore.ArchiveCard(r.Context(), agentID, cardID)
	} else {
		err = kbStore.RestoreCard(r.Context(), agentID, cardID)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": status})
}

// handleKBCardStats feeds the cards page header dashboard.
func (s *Server) handleKBCardStats(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	if agentID == "" {
		http.Error(w, "missing agent id", http.StatusBadRequest)
		return
	}
	kbStore := s.kbStoreFor(agentID)
	if kbStore == nil {
		writeJSON(w, http.StatusOK, kb.KBCardStats{})
		return
	}
	stats, err := kbStore.CardStats(r.Context(), agentID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, stats)
}
