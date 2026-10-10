package contract

import "testing"

func TestWSEchoTurn(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: WSHub,
		Spec:    sessionsWorld(),
		Body: func(r *Run) {
			id := createSession(r, "Echo")
			ws := r.WS("/ws", "ops")
			ws.Send(wsMessage{"type": "join_session", "sessionId": id})
			ws.Until("session_joined")
			ws.Send(wsMessage{"type": "send_message", "sessionId": id, "text": "hello acme"})
			ws.Until("message_complete")
		},
	})
}

func TestWSAlreadyProcessing(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: WSHub,
		Spec:    sessionsWorld(),
		Body: func(r *Run) {
			id := createSession(r, "Busy")
			ws := r.WS("/ws", "ops")
			ws.Send(wsMessage{"type": "join_session", "sessionId": id})
			ws.Until("session_joined")
			ws.Send(wsMessage{"type": "send_message", "sessionId": id, "text": "first"})
			ws.Send(wsMessage{"type": "send_message", "sessionId": id, "text": "second"})
			ws.Expect("error", nil)
		},
	})
}

func TestWSEndSessionThenSend(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: WSHub,
		Spec:    sessionsWorld(),
		Body: func(r *Run) {
			id := createSession(r, "Ended")
			ws := r.WS("/ws", "ops")
			ws.Send(wsMessage{"type": "join_session", "sessionId": id})
			ws.Until("session_joined")
			ws.Send(wsMessage{"type": "end_session", "sessionId": id})
			ws.Expect("session_state", func(f wsMessage) bool { return frameField(f, "state") == "ended" })
			ws.Send(wsMessage{"type": "send_message", "sessionId": id, "text": "anyone there"})
			ws.Expect("error", nil)
			r.HTTP("ops", "GET", "/api/sessions", nil)
		},
	})
}

func TestWSDeleteSession(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: WSHub,
		Spec:    sessionsWorld(),
		Body: func(r *Run) {
			id := createSession(r, "Doomed")
			ws := r.WS("/ws", "ops")
			ws.Send(wsMessage{"type": "join_session", "sessionId": id})
			ws.Until("session_joined")
			ws.Send(wsMessage{"type": "delete_session", "sessionId": id})
			ws.Expect("session_ended", nil)
			r.HTTP("ops", "GET", "/api/sessions", nil)
		},
	})
}
