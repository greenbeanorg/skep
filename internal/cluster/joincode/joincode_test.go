package joincode

import (
	"strings"
	"testing"
)

func node(c byte, id string) Node {
	return Node{NodeID: id, Name: "n" + string(c), Address: "10.79.20." + string(c) + ":8444", Pin: "sha256:" + strings.Repeat(string(c), 64)}
}

var (
	a = node('1', "0b5e3a52-7f0e-4c58-9d0b-3f1d6f0a9c11")
	b = node('2', "1c6f4b63-8a1f-4d69-8e1c-4a2e7a1b0d22")
)

func TestRequestRoundTripAndPasteErrors(t *testing.T) {
	line, err := EncodeRequest(Request{Node: a})
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(line, " \n\t") || !strings.HasPrefix(line, KindRequest+":1:") {
		t.Fatalf("line %q", line)
	}
	r, err := ParseRequest("  " + line + "\n")
	if err != nil || r.Node != a {
		t.Fatalf("parse %+v %v", r, err)
	}
	payload := strings.Split(line, ":")[2]
	flip := map[byte]byte{'A': 'B', 'B': 'A'}
	damaged := []byte(line)
	for i := len(KindRequest) + 3; i < len(damaged); i++ {
		if x, ok := flip[damaged[i]]; ok {
			damaged[i] = x
			break
		}
	}
	for name, bad := range map[string]string{
		"truncated":       line[:len(line)-5],
		"one character":   string(damaged),
		"no check":        KindRequest + ":1:" + payload,
		"newer version":   strings.Replace(line, ":1:", ":2:", 1),
		"an admission":    strings.Replace(line, KindRequest, KindAdmission, 1),
		"too long":        line + strings.Repeat("x", MaxRequest),
		"two lines glued": line + line,
	} {
		if _, err := ParseRequest(bad); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	// Strict content: an unknown field, or an invalid value, is refused
	// even with a correct check.
	for name, n := range map[string]Node{
		"bad pin":     {NodeID: a.NodeID, Name: a.Name, Address: a.Address, Pin: "sha256:XYZ"},
		"no port":     {NodeID: a.NodeID, Name: a.Name, Address: "10.79.20.1", Pin: a.Pin},
		"bad node ID": {NodeID: "x", Name: a.Name, Address: a.Address, Pin: a.Pin},
	} {
		raw, _ := encode(KindRequest, Request{Node: n})
		if _, err := ParseRequest(raw); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	raw, _ := encode(KindRequest, map[string]any{"node_id": a.NodeID, "name": a.Name, "address": a.Address, "pin": a.Pin, "secret": "x"})
	if _, err := ParseRequest(raw); err == nil {
		t.Error("unknown field parsed")
	}
}

func TestAdmission(t *testing.T) {
	adm := Admission{ClusterID: "7d1c2a9e-1b0f-4f3e-9a51-2c6b8e0d4f10", For: b, Members: []Node{a}, ExpiresAt: 100}
	line, err := EncodeAdmission(adm)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseAdmission(line)
	if err != nil || got.For != b || len(got.Members) != 1 || got.Members[0] != a {
		t.Fatalf("%+v %v", got, err)
	}
	// Request pasted as an admission: named as such.
	req, _ := EncodeRequest(Request{Node: a})
	if _, err := ParseAdmission(req); err == nil || !strings.Contains(err.Error(), "join-request") {
		t.Errorf("request as admission: %v", err)
	}
	for name, bad := range map[string]Admission{
		"admitted node is a member": {ClusterID: adm.ClusterID, For: a, Members: []Node{a}, ExpiresAt: 1},
		"duplicate address":         {ClusterID: adm.ClusterID, For: b, Members: []Node{a, {NodeID: "2d7a5c74-9b2a-4e7a-9f2d-5b3f8b2c1e33", Name: "x", Address: a.Address, Pin: "sha256:" + strings.Repeat("9", 64)}}, ExpiresAt: 1},
		"no members":                {ClusterID: adm.ClusterID, For: b, ExpiresAt: 1},
		"bad cluster ID":            {ClusterID: "c1", For: b, Members: []Node{a}, ExpiresAt: 1},
		"no expiry":                 {ClusterID: adm.ClusterID, For: b, Members: []Node{a}},
	} {
		if _, err := EncodeAdmission(bad); err == nil {
			t.Errorf("%s: encoded", name)
		}
		raw, _ := encode(KindAdmission, bad)
		if _, err := ParseAdmission(raw); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}
