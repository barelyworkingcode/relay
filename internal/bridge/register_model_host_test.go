package bridge

import (
	"encoding/json"
	"testing"
)

func TestRegisterModelHostRequest_Validate_HappyPath(t *testing.T) {
	r := RegisterModelHostRequest{ServiceID: "relayllm", RouterSocket: "/tmp/router.sock"}
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestRegisterModelHostRequest_Validate_RejectsEmptyFields(t *testing.T) {
	cases := []RegisterModelHostRequest{
		{ServiceID: "", RouterSocket: "/tmp/router.sock"},
		{ServiceID: "relayllm", RouterSocket: ""},
	}
	for _, r := range cases {
		if err := r.Validate(); err == nil {
			t.Errorf("%+v: validated with an empty required field", r)
		}
	}
}

func TestRegisterModelHostRequest_Validate_RejectsRelativeRouterSocket(t *testing.T) {
	cases := []string{"router.sock", "./router.sock", "relative/router.sock", "../router.sock"}
	for _, sock := range cases {
		r := RegisterModelHostRequest{ServiceID: "relayllm", RouterSocket: sock}
		if err := r.Validate(); err == nil {
			t.Errorf("router_socket %q validated despite being relative", sock)
		}
	}
}

// TestRegisterModelHostRequest_WireShapeIsSnakeCase pins the wire JSON
// shape to plan-broker-and-sessions.md §2 C8's service_id/router_socket
// spelling.
func TestRegisterModelHostRequest_WireShapeIsSnakeCase(t *testing.T) {
	raw, err := json.Marshal(RegisterModelHostRequest{ServiceID: "relayllm", RouterSocket: "/tmp/router.sock"})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["service_id"]; !ok {
		t.Errorf("wire body has no service_id key: %s", raw)
	}
	if _, ok := fields["router_socket"]; !ok {
		t.Errorf("wire body has no router_socket key: %s", raw)
	}
	if _, ok := fields["serviceId"]; ok {
		t.Errorf("wire body still carries the old camelCase key: %s", raw)
	}
}
