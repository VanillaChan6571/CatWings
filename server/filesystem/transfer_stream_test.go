package filesystem

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/pterodactyl/wings/config"
)

type transferChunkReader struct {
	r    io.Reader
	size int
}

func (r transferChunkReader) Read(p []byte) (int, error) {
	if len(p) > r.size {
		p = p[:r.size]
	}
	return r.r.Read(p)
}

func transferTestArchive(t *testing.T) ([]byte, []byte) {
	t.Helper()
	cfg := &config.Configuration{AuthenticationToken: "transfer-test"}
	cfg.System.User.Uid = os.Getuid()
	cfg.System.User.Gid = os.Getgid()
	config.Set(cfg)
	source, err := New(t.TempDir(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.UnixFS().Close() })
	payload := make([]byte, 128*1024)
	_, _ = rand.New(rand.NewSource(1)).Read(payload)
	if err := os.WriteFile(filepath.Join(source.Path(), "probe.bin"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := (&Archive{Filesystem: source}).Stream(context.Background(), &archive); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes(), payload
}

func TestExtractTransferStreamConsumesEntireArchive(t *testing.T) {
	archive, payload := transferTestArchive(t)
	expected := sha256.Sum256(archive)
	for _, size := range []int{1, 7, 4096, 32768} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			destination, err := New(t.TempDir(), 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer destination.UnixFS().Close()
			input := bytes.NewReader(archive)
			hash := sha256.New()
			if err := destination.ExtractStreamUnsafe(context.Background(), "/", io.TeeReader(transferChunkReader{input, size}, hash)); err != nil {
				t.Fatal(err)
			}
			contents, err := os.ReadFile(filepath.Join(destination.Path(), "probe.bin"))
			if err != nil || !bytes.Equal(contents, payload) {
				t.Fatal("transferred contents differ")
			}
			if input.Len() != 0 || !bytes.Equal(hash.Sum(nil), expected[:]) {
				t.Fatalf("files extracted but transfer checksum is incomplete: %d archive bytes unread", input.Len())
			}
		})
	}
}

func TestExtractTransferStreamRejectsInvalidGzipTrailer(t *testing.T) {
	archive, _ := transferTestArchive(t)
	corrupted := bytes.Clone(archive)
	corrupted[len(corrupted)-8] ^= 0xff
	for name, data := range map[string][]byte{"truncated": archive[:len(archive)-8], "corrupted": corrupted} {
		t.Run(name, func(t *testing.T) {
			destination, err := New(t.TempDir(), 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer destination.UnixFS().Close()
			if err := destination.ExtractStreamUnsafe(context.Background(), "/", transferChunkReader{bytes.NewReader(data), 1}); err == nil {
				t.Fatal("accepted an archive with an invalid gzip trailer")
			}
		})
	}
}
