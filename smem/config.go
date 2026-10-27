package smem

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"

	"synapse-mem/judge"
)

// Config is the local, per-database configuration in .smem/config.json.
// It is never synced and never holds secrets: API keys come from the
// environment only.
type Config struct {
	Judges    JudgesConfig    `json:"judges"`
	FactCheck FactCheckPolicy `json:"factcheck"`
	Checkers  []CheckerConfig `json:"checkers"`
}

// JudgesConfig chooses which judges run and when.
type JudgesConfig struct {
	Jev struct {
		// Disabled turns Jev off even when a key is present.
		Disabled bool   `json:"disabled"`
		Model    string `json:"model"`
	} `json:"jev"`
	Claude struct {
		// When is "escalate" (default: only when Jev is unsure or
		// unavailable), "always" (a second opinion on every check), or
		// "never".
		When   string `json:"when"`
		Model  string `json:"model"`
		Effort string `json:"effort"`
	} `json:"claude"`
}

// CheckerConfig declares an external checker command.
type CheckerConfig struct {
	Name    string   `json:"name"`
	Command []string `json:"command"`
	Match   string   `json:"match,omitempty"` // regexp on memory content
	Kinds   []string `json:"kinds,omitempty"`
	Timeout string   `json:"timeout,omitempty"`
}

// ConfigPath is where the config for a database lives.
func ConfigPath(dbPath string) string { return filepath.Join(filepath.Dir(dbPath), "config.json") }

// LoadConfig reads a config file. A missing file is an empty config.
func LoadConfig(path string) (*Config, error) {
	cfg := &Config{}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return cfg, nil
	} else if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Configure sets up judges, checkers and policy from cfg and the
// environment:
//
//	TYPESAFE_API_KEY (or OPENROUTER_API_KEY)  enables Jev
//	ANTHROPIC_API_KEY                         enables Claude (Haiku 5.5 by default)
//
// Jev runs first because it is fast and returns calibrated probabilities;
// Claude runs when Jev is unsure, unavailable or absent.
func (e *Engine) Configure(cfg *Config) error {
	if cfg == nil {
		cfg = &Config{}
	}
	e.FactCheckPolicy = cfg.FactCheck
	var stages []judge.Stage
	if !cfg.Judges.Jev.Disabled {
		var j *judge.Jev
		if k := os.Getenv("TYPESAFE_API_KEY"); k != "" {
			j = judge.NewJev(k)
			if u := os.Getenv("TYPESAFE_BASE_URL"); u != "" {
				j.BaseURL = u
			}
		} else if k := os.Getenv("OPENROUTER_API_KEY"); k != "" {
			j = judge.NewJevOpenRouter(k)
		}
		if j != nil {
			if cfg.Judges.Jev.Model != "" {
				j.Model = cfg.Judges.Jev.Model
			}
			stages = append(stages, judge.Stage{Judge: j})
		}
	}
	when := cfg.Judges.Claude.When
	if when == "" {
		when = "escalate"
	}
	if when != "never" && os.Getenv("ANTHROPIC_API_KEY") != "" {
		c := judge.NewClaude(option.WithAPIKey(os.Getenv("ANTHROPIC_API_KEY")))
		if cfg.Judges.Claude.Model != "" {
			c.Model = cfg.Judges.Claude.Model
		}
		if cfg.Judges.Claude.Effort != "" {
			c.Effort = anthropic.OutputConfigEffort(cfg.Judges.Claude.Effort)
		}
		stages = append(stages, judge.Stage{Judge: c, Always: when == "always"})
	}
	e.Judges = judge.Cascade{Stages: stages}

	e.Checkers = nil
	for _, cc := range cfg.Checkers {
		if cc.Name == "" || len(cc.Command) == 0 {
			return fmt.Errorf("checker needs a name and a command")
		}
		c := &CommandChecker{CheckName: cc.Name, Command: cc.Command, Kinds: cc.Kinds,
			Dir: filepath.Dir(filepath.Dir(e.path))}
		if cc.Match != "" {
			re, err := regexp.Compile(cc.Match)
			if err != nil {
				return fmt.Errorf("checker %s: %w", cc.Name, err)
			}
			c.Match = re
		}
		if cc.Timeout != "" {
			d, err := time.ParseDuration(cc.Timeout)
			if err != nil {
				return fmt.Errorf("checker %s: %w", cc.Name, err)
			}
			c.Timeout = d
		}
		e.Checkers = append(e.Checkers, c)
	}
	return nil
}
