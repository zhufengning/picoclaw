package channels

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
)

func TestOneBotDescribeImage_UsesHiCompatibleCache(t *testing.T) {
	msgBus := bus.NewMessageBus()
	ch, err := NewOneBotChannel(config.OneBotConfig{}, msgBus)
	if err != nil {
		t.Fatalf("NewOneBotChannel() error = %v", err)
	}

	tempDir := t.TempDir()
	imagePath := filepath.Join(tempDir, "a.jpg")
	if err := os.WriteFile(imagePath, []byte("fake-image"), 0644); err != nil {
		t.Fatalf("write image failed: %v", err)
	}
	cachePath := imagePath + ".txt"
	want := "缓存命中描述"
	if err := os.WriteFile(cachePath, []byte(want), 0644); err != nil {
		t.Fatalf("write cache failed: %v", err)
	}

	got := ch.describeImage(imagePath)
	if got != want {
		t.Fatalf("describeImage() = %q, want %q", got, want)
	}
}

func TestOneBotDescribeImage_WritesHiCompatibleCache(t *testing.T) {
	msgBus := bus.NewMessageBus()
	ch, err := NewOneBotChannel(config.OneBotConfig{
		ImageCaptionProvider: "openai",
		ImageCaptionModel:    "gpt-4o-mini",
	}, msgBus)
	if err != nil {
		t.Fatalf("NewOneBotChannel() error = %v", err)
	}

	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"这是一张测试图片"}}]}`))
	}))
	defer server.Close()

	ch.SetProvidersConfig(config.ProvidersConfig{
		OpenAI: config.ProviderConfig{
			APIKey:  "test-key",
			APIBase: server.URL + "/v1",
		},
	})

	tempDir := t.TempDir()
	imagePath := filepath.Join(tempDir, "b.jpg")
	if err := os.WriteFile(imagePath, []byte("fake-image"), 0644); err != nil {
		t.Fatalf("write image failed: %v", err)
	}

	got1 := ch.describeImage(imagePath)
	got2 := ch.describeImage(imagePath)
	if got1 == "" || got2 == "" {
		t.Fatalf("describeImage() should return non-empty summary, got1=%q got2=%q", got1, got2)
	}
	if got1 != got2 {
		t.Fatalf("cached summary mismatch: got1=%q got2=%q", got1, got2)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("caption API call count = %d, want 1", calls)
	}

	cacheData, err := os.ReadFile(imagePath + ".txt")
	if err != nil {
		t.Fatalf("read cache file failed: %v", err)
	}
	if string(cacheData) != got1 {
		t.Fatalf("cache content = %q, want %q", string(cacheData), got1)
	}
}

func TestOneBotEnsureImageInWorkspace_CopiesToWorkspaceTmpImgs(t *testing.T) {
	msgBus := bus.NewMessageBus()
	ch, err := NewOneBotChannel(config.OneBotConfig{}, msgBus)
	if err != nil {
		t.Fatalf("NewOneBotChannel() error = %v", err)
	}

	workspace := t.TempDir()
	ch.SetWorkspacePath(workspace)

	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "orig.jpg")
	wantBytes := []byte("image-bytes")
	if err := os.WriteFile(srcPath, wantBytes, 0644); err != nil {
		t.Fatalf("write source image failed: %v", err)
	}

	dstPath := ch.ensureImageInWorkspace(srcPath, "orig.jpg")
	if !strings.HasPrefix(dstPath, filepath.Join(workspace, "tmp", "imgs")+string(filepath.Separator)) {
		t.Fatalf("dst path = %q, want under workspace tmp/imgs", dstPath)
	}

	gotBytes, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatalf("read copied image failed: %v", err)
	}
	if string(gotBytes) != string(wantBytes) {
		t.Fatalf("copied content mismatch: got=%q want=%q", string(gotBytes), string(wantBytes))
	}
}

func TestOneBotJoinURL_AvoidsDuplicateVersionPath(t *testing.T) {
	cases := []struct {
		base string
		want string
	}{
		{base: "https://api.openai.com/v1", want: "https://api.openai.com/v1/chat/completions"},
		{base: "https://openrouter.ai/api/v1", want: "https://openrouter.ai/api/v1/chat/completions"},
		{base: "https://api.groq.com/openai/v1", want: "https://api.groq.com/openai/v1/chat/completions"},
		{base: "https://example.com/chat/completions", want: "https://example.com/chat/completions"},
	}

	for _, tc := range cases {
		got := oneBotJoinURL(tc.base, "/chat/completions")
		if got != tc.want {
			t.Fatalf("oneBotJoinURL(%q) = %q, want %q", tc.base, got, tc.want)
		}
	}
}
