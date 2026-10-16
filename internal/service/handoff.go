package service

import (
	"encoding/json"
	"fmt"

	"github.com/yebai/better-download-manager/internal/config"
	"github.com/yebai/better-download-manager/internal/store"
	"github.com/yebai/better-download-manager/internal/takeover"
)

func (s *DownloadService) acceptHandoff(req takeover.DownloadRequest) error {
	s.handoffMu.Lock()
	defer s.handoffMu.Unlock()
	if req.RequestID == "" {
		req.RequestID = newID()
	}
	h, exists, err := s.store.Handoff(req.RequestID)
	if err != nil {
		return err
	}
	if exists && h.State != "failed" {
		return nil
	}
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	h = store.Handoff{ID: req.RequestID, Request: data, State: "processing"}
	if err := s.store.SaveHandoff(h); err != nil {
		return err
	}
	if s.GetSettings().TakeoverAction == config.ActionShowDialog {
		h.State = "dialog"
		if err := s.store.SaveHandoff(h); err != nil {
			return err
		}
	}
	if err := s.dispatchTakeover(req); err != nil {
		h.State, h.Error = "failed", err.Error()
		_ = s.store.SaveHandoff(h)
		return err
	}
	if h.State != "dialog" {
		h.State = "task"
		h.Request = nil
	}
	current, found, err := s.store.Handoff(h.ID)
	if err != nil {
		return err
	}
	if found && current.State == "task" {
		return nil
	}
	return s.store.SaveHandoff(h)
}

func (s *DownloadService) handoffStatus(id string) (takeover.Receipt, error) {
	h, exists, err := s.store.Handoff(id)
	if err != nil {
		return takeover.Receipt{}, err
	}
	r := takeover.Receipt{OK: true, RequestID: id, State: "missing"}
	if !exists {
		return r, nil
	}
	r.State = h.State
	_, taskErr := s.engine.Get("handoff-" + id)
	r.Accepted = h.State == "dialog" || h.State == "task" || h.State == "cancelled" || taskErr == nil
	return r, nil
}

func (s *DownloadService) CancelHandoff(id string) error {
	if id == "" {
		return nil
	}
	s.handoffMu.Lock()
	defer s.handoffMu.Unlock()
	h, found, err := s.store.Handoff(id)
	if err != nil || !found {
		return err
	}
	if h.State == "dialog" {
		h.State = "cancelled"
		h.Request = nil
		return s.store.SaveHandoff(h)
	}
	return nil
}

func (s *DownloadService) restoreHandoffs() error {
	handoffs, err := s.store.PendingHandoffs()
	if err != nil {
		return err
	}
	for _, h := range handoffs {
		if _, err := s.engine.Get("handoff-" + h.ID); err == nil {
			h.State = "task"
			if err := s.store.SaveHandoff(h); err != nil {
				return err
			}
			continue
		}
		var req takeover.DownloadRequest
		if err := json.Unmarshal(h.Request, &req); err != nil {
			return fmt.Errorf("restore handoff: %w", err)
		}
		if err := s.dispatchTakeover(req); err != nil {
			return err
		}
	}
	return nil
}
