package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// handleRoomZone moves a room to another heating channel. The hub refuses a
// room on two channels at once, so the room is removed from its old channel
// first, then added to the new one; if the add fails the removal is undone.
func (s *Server) handleRoomZone(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Room    int `json:"room"`
		Channel int `json:"channel"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	if err := s.moveRoom(ctx, req.Room, req.Channel); err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

type channelRooms struct {
	ID      int   `json:"id"`
	RoomIds []int `json:"RoomIds"`
}

func (s *Server) moveRoom(ctx context.Context, room, target int) error {
	res := s.hub.Do(ctx, "GET", "/data/domain/HeatingChannel", nil, true)
	if res.Err != nil || res.Status != 200 {
		return fmt.Errorf("reading channels: %v (HTTP %d)", res.Err, res.Status)
	}
	var chans []channelRooms
	if err := json.Unmarshal(res.Body, &chans); err != nil {
		return fmt.Errorf("reading channels: %v", err)
	}
	var from, to *channelRooms
	for i := range chans {
		if chans[i].ID == target {
			to = &chans[i]
		}
		for _, id := range chans[i].RoomIds {
			if id == room {
				from = &chans[i]
			}
		}
	}
	if to == nil {
		return fmt.Errorf("no heating channel %d", target)
	}
	if from != nil && from.ID == target {
		return nil
	}
	if from != nil {
		if err := s.setChannelRooms(ctx, from.ID, without(from.RoomIds, room)); err != nil {
			return fmt.Errorf("removing room from channel %d: %v", from.ID, err)
		}
	}
	if err := s.setChannelRooms(ctx, to.ID, append(append([]int{}, to.RoomIds...), room)); err != nil {
		if from != nil {
			if rerr := s.setChannelRooms(ctx, from.ID, from.RoomIds); rerr != nil {
				return fmt.Errorf("adding to channel %d failed (%v) and restoring channel %d also failed (%v)", to.ID, err, from.ID, rerr)
			}
		}
		return fmt.Errorf("adding room to channel %d: %v (nothing changed)", to.ID, err)
	}
	return nil
}

func (s *Server) setChannelRooms(ctx context.Context, channel int, rooms []int) error {
	body, _ := json.Marshal(map[string][]int{"RoomIds": rooms})
	path := "/data/domain/HeatingChannel/" + strconv.Itoa(channel)
	res := s.hub.Do(ctx, "PATCH", path, body, true)
	if res.Err != nil {
		return res.Err
	}
	if res.Status < 200 || res.Status >= 300 {
		return fmt.Errorf("HTTP %d: %s", res.Status, res.Body)
	}
	if sec := s.store.MergeWrite(path, res.Body); sec != "" {
		s.poller.Refresh(sec)
	}
	return nil
}

func without(xs []int, x int) []int {
	out := []int{}
	for _, v := range xs {
		if v != x {
			out = append(out, v)
		}
	}
	return out
}
