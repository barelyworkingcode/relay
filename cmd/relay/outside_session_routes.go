package main

import (
	"errors"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/outsidesessions"
	"github.com/gorilla/websocket"
	"net/http"
	"time"
)

func RegisterOutsideSessionRoutes(rr *control.RouteRegistrar, i *outsidesessions.Inventory) {
	rr.Handle(control.ClassChiefOfStaff, "GET /api/chief-of-staff/outside-sessions", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, i.Snapshot()) })
	rr.Handle(control.ClassChiefOfStaff, "GET /api/chief-of-staff/outside-sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		detail, err := i.Detail(r.Context(), r.PathValue("id"))
		if err != nil {
			status, code := http.StatusBadGateway, "outside_status_unavailable"
			if errors.Is(err, outsidesessions.ErrNotFound) {
				status, code = http.StatusNotFound, "outside_session_not_found"
			}
			writeChiefOfStaffError(w, status, code, "outside session status could not be read")
			return
		}
		writeJSON(w, http.StatusOK, detail)
	})
	rr.Handle(control.ClassChiefOfStaff, "GET /ws/outside-sessions", func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		snapshots, unsubscribe := i.Subscribe()
		defer unsubscribe()
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				kind, _, err := conn.NextReader()
				if err != nil {
					return
				}
				if kind == websocket.TextMessage || kind == websocket.BinaryMessage {
					_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "outside sessions are read-only"), time.Now().Add(time.Second))
					return
				}
			}
		}()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-done:
				return
			case s := <-snapshots:
				conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
				if conn.WriteJSON(s) != nil {
					return
				}
			}
		}
	})
}
