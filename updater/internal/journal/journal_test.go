package journal

import (
	"strings"
	"testing"
)

func TestIdentityIgnoresAuditVersion(t *testing.T) {
	a := Manifest{SchemaVersion: 1, Digest: "sha256:a", SupportedMinecraft: []string{"1.21.1"}, ConfigMapResourceVersion: "1"}
	b := a
	b.ConfigMapResourceVersion = "2"
	if !SameIdentity(a, b) {
		t.Fatal("audit field changed identity")
	}
	b.Digest = "sha256:b"
	if SameIdentity(a, b) {
		t.Fatal("digest ignored")
	}
}

func TestStrictDecoder(t *testing.T) {
	for _, text := range []string{`{"schemaVersion":1,"unknown":true}`, `{}`, `{"schemaVersion":2}`, `{"schemaVersion":1,"schemaVersion":1}`, `{"schemaVersion":1} {}`} {
		if _, err := Decode[Flag](strings.NewReader(text)); err == nil {
			t.Errorf("accepted %s", text)
		}
	}
}

func TestFailureReasons(t *testing.T) {
	for _, reason := range []string{"gate-ack-unrecoverable", "pod-or-pvc-unavailable", "init-unrecoverable", "invariant-violation", "whitelist-restore-failed", "persisted-whitelist-cas-conflict"} {
		j := Journal{}
		if err := j.Fail(reason); err != nil {
			t.Fatal(err)
		}
		if j.Outcome == nil || *j.Outcome != "FAILED_MANUAL_INTERVENTION" || j.FailureReason == nil || *j.FailureReason != reason {
			t.Fatal(j)
		}
	}
	if err := new(Journal).Fail("random"); err == nil {
		t.Fatal("accepted unenumerated reason")
	}
}
