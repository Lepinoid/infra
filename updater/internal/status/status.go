package status

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/lepinoid/infra/updater/internal/gate"
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
	if !regexp.MustCompile(`^[0-9]+\.[0-9]+(?:\.[0-9]+)?$`).MatchString(version) {
		return Monitor{}
	}
	return Monitor{version, true}
}

func (s Status) Minecraft(m Monitor, pod string, now time.Time) string {
	if s.SchemaVersion != 1 || !m.Valid || s.MinecraftVersion == "" || s.MinecraftVersion != m.Version || s.ServerInstanceID != pod || !gate.Fresh(now, s.UpdatedAt) {
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
	return s.SchemaVersion == 1 && want.PodUID != "" && want.PodUID != want.PreviousPodUID && s.ServerInstanceID == want.PodUID && !s.ProcessStartedAt.Before(want.RequestedAt) && !s.ProcessStartedAt.After(now.Add(2*time.Second)) && gate.Fresh(now, s.UpdatedAt) && s.Phase == "HEALTHY" && s.PluginVersion == want.Manifest.Version && s.PluginCommitSHA == want.Manifest.PluginCommitSHA && s.Multiverse.Compatible && s.Multiverse.Detected != nil && *s.Multiverse.Detected == want.Manifest.MultiverseVersion && s.Multiverse.Expected == want.Manifest.MultiverseVersion
}

var checkpoint = regexp.MustCompile(`^operationId=([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)

func Checkpoint(text string) (string, string) {
	text = strings.TrimSpace(text)
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
