package status

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/lepinoid/infra/updater/internal/journal"
)

type Status struct {
	SchemaVersion    int        `json:"schemaVersion"`
	ServerInstanceID string     `json:"serverInstanceId"`
	ProcessStartedAt time.Time  `json:"processStartedAt"`
	StatusSequence   int64      `json:"statusSequence"`
	Phase            string     `json:"phase"`
	PluginVersion    string     `json:"pluginVersion"`
	PluginCommitSHA  string     `json:"pluginCommitSha"`
	MinecraftVersion string     `json:"serverMinecraftVersion"`
	Multiverse       Multiverse `json:"multiverse"`
	AutoCommit       AutoCommit `json:"autoCommit"`
	UpdatedAt        time.Time  `json:"updatedAt"`
}

type Multiverse struct {
	Detected   *string `json:"detected"`
	Expected   string  `json:"expected"`
	Compatible bool    `json:"compatible"`
}
type AutoCommit struct {
	OperationID     *string    `json:"operationId"`
	Trigger         *string    `json:"trigger"`
	State           string     `json:"state"`
	Result          *string    `json:"result"`
	StartedAt       *time.Time `json:"startedAt"`
	CompletedAt     *time.Time `json:"completedAt"`
	PushedCommitSHA *string    `json:"pushedCommitSha"`
}
type Monitor struct {
	Version string
	Valid   bool
}

func ParseMonitor(data []byte) Monitor {
	var wire struct {
		ServerInfo *struct {
			Version *struct {
				Name string `json:"name"`
			} `json:"version"`
		} `json:"server_info"`
	}
	if err := json.Unmarshal(data, &wire); err != nil || wire.ServerInfo == nil || wire.ServerInfo.Version == nil {
		return Monitor{}
	}
	version := wire.ServerInfo.Version.Name
	// mc-monitor は現在の itzg イメージでは version.name に "Paper 1.21.8" のように
	// 実装名を前置して返すため、末尾の x.y(.z) を抜き出して比較対象にする。
	m := regexp.MustCompile(`([0-9]+\.[0-9]+(?:\.[0-9]+)?)\s*$`).FindStringSubmatch(version)
	if m == nil {
		return Monitor{}
	}
	return Monitor{m[1], true}
}

func (s Status) Minecraft(m Monitor, pod string, now time.Time) string {
	// 鮮度（updatedAt の再近接）は要求しない: プラグイン側の UpdaterStatusManager は
	// イベント駆動で heartbeat を持たず、起動后はファイルが古いだけで実態は alive のため。
	// サーバーが死んでいる場合は monitor 応答が取れず monitor 側で失格する。
	if s.SchemaVersion != 1 || !m.Valid || s.MinecraftVersion == "" || s.MinecraftVersion != m.Version || s.ServerInstanceID != pod {
		return ""
	}
	return m.Version
}

type Startup struct {
	PodUID         string
	PreviousPodUID string
	RequestedAt    time.Time
	Manifest       journal.Manifest
}

func (s Status) Healthy(want Startup, now time.Time) bool {
	// UpdaterStatusManager writes on events, not on a heartbeat. Process and
	// Pod identity bind this observation to the requested restart; a live
	// monitor is checked by the caller. Gate acknowledgments retain freshness.
	validTimes := !want.RequestedAt.IsZero() && !s.ProcessStartedAt.IsZero() && !s.UpdatedAt.IsZero() && !s.ProcessStartedAt.Before(want.RequestedAt) && !s.UpdatedAt.Before(s.ProcessStartedAt) && !s.ProcessStartedAt.After(now.Add(2*time.Second)) && !s.UpdatedAt.After(now.Add(2*time.Second))
	return validTimes && s.SchemaVersion == 1 && s.StatusSequence >= 0 && want.PodUID != "" && want.PreviousPodUID != "" && want.PodUID != want.PreviousPodUID && s.ServerInstanceID == want.PodUID && s.Phase == "HEALTHY" && s.PluginVersion == want.Manifest.Version && s.PluginCommitSHA == want.Manifest.PluginCommitSHA && slices.Contains(want.Manifest.SupportedMinecraft, s.MinecraftVersion) && s.Multiverse.Compatible && s.Multiverse.Detected != nil && *s.Multiverse.Detected == want.Manifest.MultiverseVersion && s.Multiverse.Expected == want.Manifest.MultiverseVersion
}

var checkpoint = regexp.MustCompile(`^operationId=([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)

func Checkpoint(text string) (string, string) {
	// Paper appends a newline to Bukkit command replies. rcon-cli 1.7.7
	// renders that newline as "\n\x1b[0m" on Unix, even without a TTY.
	// Remove only its terminal reset; embedded escapes and extra response
	// lines must still fail the exact protocol match below.
	text = strings.TrimSpace(text)
	text = strings.TrimSpace(strings.TrimSuffix(text, "\n\x1b[0m"))
	switch text {
	case "error=busy":
		return "", "checkpoint-busy"
	case "error=unavailable":
		return "", "checkpoint-unavailable"
	default:
		if m := checkpoint.FindStringSubmatch(text); m != nil {
			return m[1], ""
		}
		return "", "checkpoint-invalid-response"
	}
}
