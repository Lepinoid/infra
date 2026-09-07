package artifact

import (
	"archive/tar"
	"bytes"
	"testing"
)

func TestRejectUnsafeArchive(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind byte
	}{{"../LepinoidTools.jar", tar.TypeReg}, {"/LepinoidTools.jar", tar.TypeReg}, {"LepinoidTools.jar", tar.TypeSymlink}, {"unexpected", tar.TypeReg}} {
		var b bytes.Buffer
		w := tar.NewWriter(&b)
		if err := w.WriteHeader(&tar.Header{Name: tc.name, Typeflag: tc.kind, Mode: 0644}); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if err := Extract(&b, t.TempDir()); err == nil {
			t.Fatalf("accepted %+v", tc)
		}
	}
}
