package provider

import (
	"bufio"
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

const piModelsCacheTTL = 5 * time.Minute

type piModelsCache struct {
	mu        sync.Mutex
	models    []sessionstypes.ModelInfo
	expiresAt time.Time
	binPath   string
}

var piModels piModelsCache

// FetchPiModels returns the list of models pi's own `--list-models` reports,
// wrapped as ModelInfo entries with the `pi/<provider>/<modelId>` value
// convention. Unlike relayLLM's version, this does not merge in a
// project-overlay's provider list or synthesize relay-router entries: a
// relay-brokered pi session now exposes exactly one provider/model (see
// pioverlay's doc comment), which the session already knows without
// needing a catalog call — this function exists for surfacing what pi's own
// global config additionally offers, nothing more.
//
// The upstream exec is cached for 5 minutes. Returns nil if pi is not on
// PATH — callers should drop the pi section from a model listing silently.
func FetchPiModels(ctx context.Context, binaryPath string) []sessionstypes.ModelInfo {
	return fetchPiListModelsCached(ctx, binaryPath)
}

func fetchPiListModelsCached(ctx context.Context, configuredPath string) []sessionstypes.ModelInfo {
	piPath := resolvePiPath(configuredPath)

	piModels.mu.Lock()
	if !piModels.expiresAt.IsZero() &&
		time.Now().Before(piModels.expiresAt) &&
		piModels.binPath == piPath {
		cached := piModels.models
		piModels.mu.Unlock()
		return cached
	}
	piModels.mu.Unlock()

	if _, err := os.Stat(piPath); err != nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, piPath, "--list-models")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		slog.Warn("pi --list-models failed", "error", err)
		return nil
	}

	models := parsePiListModels(out.Bytes())

	piModels.mu.Lock()
	piModels.models = models
	piModels.expiresAt = time.Now().Add(piModelsCacheTTL)
	piModels.binPath = piPath
	piModels.mu.Unlock()

	return models
}

// parsePiListModels scans `pi --list-models` output. Pi prints a
// fixed-width, space-padded table whose model column can contain spaces, so
// this locates the header's column offsets and slices each row against
// them rather than tokenizing on whitespace.
func parsePiListModels(raw []byte) []sessionstypes.ModelInfo {
	seen := make(map[string]bool)
	var models []sessionstypes.ModelInfo

	var providerStart, modelStart, modelEnd int
	headerParsed := false

	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := stripANSI(scanner.Text())
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !headerParsed {
			providerStart = strings.Index(line, "provider")
			modelStart = strings.Index(line, "model")
			modelEnd = strings.Index(line, "context")
			if providerStart < 0 || modelStart <= providerStart || modelEnd <= modelStart {
				return nil
			}
			headerParsed = true
			continue
		}

		if len(line) <= modelStart {
			continue
		}
		provider := strings.ToLower(strings.TrimSpace(line[providerStart:modelStart]))
		end := modelEnd
		if end > len(line) {
			end = len(line)
		}
		modelID := strings.TrimSpace(line[modelStart:end])
		if !looksLikeProvider(provider) || !looksLikeModelName(modelID) {
			continue
		}
		value := "pi/" + provider + "/" + modelID
		if seen[value] {
			continue
		}
		seen[value] = true
		models = append(models, sessionstypes.ModelInfo{
			Label:    value,
			Value:    value,
			Group:    "Pi · " + provider,
			Provider: "pi",
		})
	}
	return models
}

func looksLikeProvider(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-':
		default:
			return false
		}
	}
	return true
}

func looksLikeModelName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.' || r == ':' || r == '/' || r == ' ':
		default:
			return false
		}
	}
	return true
}

func stripANSI(s string) string {
	var b strings.Builder
	inEsc := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inEsc {
			if c >= 0x40 && c <= 0x7e {
				inEsc = false
			}
			continue
		}
		if c == 0x1b {
			inEsc = true
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}
