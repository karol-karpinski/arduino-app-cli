// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package app

import (
	"fmt"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/spf13/cobra"

	"github.com/arduino/arduino-app-cli/cmd/feedback"
	"github.com/arduino/arduino-app-cli/internal/orchestrator/peripherals"
	"github.com/arduino/arduino-app-cli/internal/tablestyle"
)

func newAudioCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "audio",
		Short: "List available audio sinks and sources on the host",
		Long:  "List available PipeWire audio sinks (outputs) and sources (inputs). Use the sink index with --output when starting an app.",
		RunE: func(cmd *cobra.Command, args []string) error {
			devices, err := peripherals.GetAudioDevices()
			if err != nil {
				return fmt.Errorf("unable to list audio devices: %w", err)
			}
			feedback.PrintResult(audioListResult{devices: devices})
			return nil
		},
	}
}

type audioListResult struct {
	devices peripherals.AudioDevices
}

type audioDeviceJSON struct {
	Index       int      `json:"index"`
	PipeWireID  int      `json:"pipewire_id"`
	DisplayName string   `json:"display_name"`
	Default     bool     `json:"default"`
	Processes   []string `json:"processes"`
}

type audioListJSON struct {
	Sinks   []audioDeviceJSON `json:"sinks"`
	Sources []audioDeviceJSON `json:"sources"`
}

func (r audioListResult) Data() any {
	toJSON := func(sinks []peripherals.AudioSink) []audioDeviceJSON {
		out := make([]audioDeviceJSON, len(sinks))
		for i, s := range sinks {
			out[i] = audioDeviceJSON{
				Index:       s.ListIndex,
				PipeWireID:  s.PipeWireID,
				DisplayName: s.DisplayName,
				Default:     s.IsDefault,
				Processes:   s.Processes,
			}
		}
		return out
	}
	return audioListJSON{
		Sinks:   toJSON(r.devices.Sinks),
		Sources: toJSON(r.devices.Sources),
	}
}

func (r audioListResult) String() string {
	var b strings.Builder

	renderTable := func(title string, items []peripherals.AudioSink) {
		b.WriteString(title + "\n")
		if len(items) == 0 {
			b.WriteString("  (none)\n")
			return
		}
		t := table.NewWriter()
		t.SetStyle(tablestyle.CustomCleanStyle)
		t.AppendHeader(table.Row{"INDEX", "DEFAULT", "NAME", "PIPEWIRE ID", "PROCESSES"})
		for _, s := range items {
			def := ""
			if s.IsDefault {
				def = "*"
			}
			t.AppendRow(table.Row{s.ListIndex, def, s.DisplayName, s.PipeWireID, strings.Join(s.Processes, ", ")})
		}
		b.WriteString(t.Render())
		b.WriteString("\n")
	}

	renderTable("Sinks (outputs) — use INDEX with --output:", r.devices.Sinks)
	b.WriteString("\n")
	renderTable("Sources (inputs):", r.devices.Sources)
	return b.String()
}
