package push

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateIOBoundsPermissionsAndSymlinks(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	config, state := filepath.Join(dir, "config.json"), filepath.Join(dir, "state.json")
	if err := savePrivate(config, map[string]string{"synthetic": "test"}); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if readPrivate(config, 64<<10, &got) != nil {
		t.Fatal("valid private input rejected")
	}
	if readPrivate(config, 4, &got) == nil {
		t.Fatal("size limit ignored")
	}
	if os.Chmod(config, 0644) != nil {
		t.Fatal("chmod")
	}
	if readPrivate(config, 64<<10, &got) == nil {
		t.Fatal("public input accepted")
	}
	if err := savePrivate(state, map[string]string{"token": "original"}); err != nil {
		t.Fatal(err)
	}
	original, _ := os.ReadFile(state)
	if err := savePrivate(state, make(chan int)); err == nil {
		t.Fatal("bad JSON accepted")
	}
	after, _ := os.ReadFile(state)
	if !bytes.Equal(original, after) {
		t.Fatal("failed save destroyed original")
	}
	if err := os.Chmod(state, 0644); err != nil {
		t.Fatal(err)
	}
	if savePrivate(state, map[string]string{"token": "replacement"}) == nil {
		t.Fatal("public state overwritten")
	}
	after, _ = os.ReadFile(state)
	if !bytes.Equal(original, after) {
		t.Fatal("unsafe save destroyed original")
	}
	link := filepath.Join(filepath.Dir(state), "link.json")
	if err := os.Symlink(state, link); err != nil {
		t.Fatal(err)
	}
	if readPrivate(link, 1<<20, &got) == nil || savePrivate(link, got) == nil {
		t.Fatal("file symlink accepted")
	}
	ancestor := filepath.Join(filepath.Dir(state), "linked-dir")
	if err := os.Symlink(filepath.Dir(state), ancestor); err != nil {
		t.Fatal(err)
	}
	if readPrivate(filepath.Join(ancestor, "config.json"), 64<<10, &got) == nil || savePrivate(filepath.Join(ancestor, "new.json"), got) == nil {
		t.Fatal("ancestor symlink accepted")
	}
	nested := filepath.Join(filepath.Dir(state), "new", "private", "state.json")
	if err := savePrivate(nested, map[string]bool{"ok": true}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Dir(nested), filepath.Dir(filepath.Dir(nested))} {
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0700 {
			t.Fatal("new directory permissions")
		}
	}
	info, _ := os.Stat(nested)
	if info.Mode().Perm() != 0600 {
		t.Fatal("new file permissions")
	}
	if err := os.Chmod(filepath.Dir(nested), 0755); err != nil {
		t.Fatal(err)
	}
	if readPrivate(nested, 1<<20, &got) == nil || savePrivate(nested, got) == nil {
		t.Fatal("public parent accepted")
	}
}
