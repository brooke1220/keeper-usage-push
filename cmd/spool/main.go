// Command spool inspects or requeues locally quarantined records without uploading.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/brooke1220/keeper-usage-push/push"
	"gopkg.in/yaml.v3"
)

func run() error {
	configPath := flag.String("config", "", "plugin configuration YAML (without the CPA plugins wrapper)")
	retry := flag.String("retry", "", "requeue this delivery event ID after fixing the cause")
	retryCode := flag.String("retry-code", "", "requeue every record quarantined with this error code after fixing the cause")
	flag.Parse()
	if *retry != "" && *retryCode != "" {
		return fmt.Errorf("use --retry or --retry-code, not both")
	}
	raw, err := os.ReadFile(*configPath)
	if err != nil {
		return err
	}
	var cfg push.Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	q, err := push.Open(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = q.Close() }()
	var requeued int64
	if *retry != "" {
		if err := q.Replay(*retry); err != nil {
			return err
		}
		requeued = 1
	}
	if *retryCode != "" {
		if requeued, err = q.ReplayCode(*retryCode); err != nil {
			return err
		}
	}
	stats, err := q.Stats()
	if err != nil {
		return err
	}
	codes, err := q.QuarantineCodes()
	if err != nil {
		return err
	}
	records, err := q.Quarantined()
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(struct {
		Stats       push.Stats              `json:"stats"`
		Requeued    int64                   `json:"requeued"`
		Codes       map[string]int64        `json:"quarantined_by_code"`
		Quarantined []push.QuarantinedEvent `json:"quarantined_first_100"`
	}{stats, requeued, codes, records})
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
