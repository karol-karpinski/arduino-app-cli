// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package peripherals

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/arduino/go-paths-helper"

	linuxconfig "github.com/arduino/arduino-app-cli/internal/orchestrator/linuxConfig"
)

type AvailableDevices struct {
	HasVideoDevice     bool
	HasSoundDevice     bool
	HasGPUDevice       bool
	HasCSICameraDevice bool
}

type DeviceClass string

const (
	CameraClass     DeviceClass = "camera"
	MicrophoneClass DeviceClass = "microphone"
	SpeakerClass    DeviceClass = "speaker"
)

func Detect(ctx context.Context) (AvailableDevices, error) {
	res := AvailableDevices{}

	deviceList, err := paths.New("/dev").ReadDir()
	if err != nil {
		slog.Error("unable to list /dev", slog.String("error", err.Error()))
		return AvailableDevices{}, fmt.Errorf("unable to list board devices")
	}

	for _, p := range deviceList {
		if p.HasPrefix("dri") {
			res.HasGPUDevice = true
		}
	}

	// Verify if there are real video devices (cameras) in /dev/v4l/by-id
	if camDevices := GetVideoDevices(); len(camDevices) > 0 {
		res.HasVideoDevice = true
	}
	// Verify if there are real sound devices in /dev/snd/by-id
	if sndDev := GetSoundDevices(); sndDev > 0 {
		res.HasSoundDevice = true
	}

	carriers, err := linuxconfig.GetEnabledCarriers(ctx)
	if err != nil {
		slog.Warn("unable to get enabled devices from linux config", slog.String("error", err.Error()))
	}
	res.HasCSICameraDevice = HasCSICamera(carriers)

	return res, nil
}

func GetSoundDevices() int {
	// Check and read /dev/snd. This fs contains only real sound devices
	soundDevicePath := paths.New("/dev/snd/by-id")
	if _, err := soundDevicePath.Stat(); err != nil {
		return 0 // no sound device found
	}
	sndDeviceList, err := soundDevicePath.ReadDir()
	if err != nil {
		slog.Warn("unable to list /dev/snd/by-id", slog.String("error", err.Error()))
		return 0
	}
	return len(sndDeviceList)
}

func GetVideoDevices() map[int]string {
	// Check and read /dev/v4l/by-id. This fs contains only real video devices (cameras), filtering out devices for HW acceleration (like Qualcomm Venus)
	videoDevicePath := paths.New("/dev/v4l/by-id")
	if _, err := videoDevicePath.Stat(); err != nil {
		return nil // no video device found
	}
	v4DeviceList, err := videoDevicePath.ReadDir()
	if err != nil {
		slog.Warn("unable to list /dev/v4l/by-id", slog.String("error", err.Error()))
		return nil
	}
	sortedDevices := []string{}
	for _, v4d := range v4DeviceList {
		sortedDevices = append(sortedDevices, v4d.String())
	}
	sortV4lByIndexDevices(sortedDevices)

	camDevices := []string{}
	for _, v4d := range sortedDevices {
		if linked, err := os.Readlink(v4d); err == nil {
			split := strings.Split(linked, "/")
			realVideoDev := filepath.Join("/dev", split[len(split)-1])
			slog.Debug("found v4l device", slog.String("device", v4d), slog.String("linked", linked), slog.String("realDevice", realVideoDev))
			camDevices = append(camDevices, realVideoDev)
		} else {
			slog.Warn("unable to readlink v4l device", slog.String("device", v4d), slog.String("error", err.Error()))
		}
	}
	// VIDEO_DEVICE will be the first device in /dev/v4l/by-id
	slog.Debug("sorted camera devices", slog.Any("devices", camDevices))
	deviceMap := map[int]string{}
	for i, cam := range camDevices {
		slog.Debug("found camera device", slog.Int("index", i), slog.String("device", cam))
		deviceMap[i] = cam
	}
	return deviceMap
}

func sortV4lByIndexDevices(deviceList []string) {
	slices.SortFunc(deviceList, func(a, b string) int {
		// Extract the index from the first string
		indexI, err := extractIndexFromVideoDeviceName(a)
		if err != nil {
			return 0
		}

		// Extract the index from the second string
		indexJ, err := extractIndexFromVideoDeviceName(b)
		if err != nil {
			return 0
		}

		// Compare the numeric indices
		switch {
		case indexI < indexJ:
			return -1
		case indexI > indexJ:
			return 1
		default:
			return 0
		}
	})
}

func extractIndexFromVideoDeviceName(device string) (int, error) {
	idx := strings.LastIndex(device, "index")

	if idx == -1 {
		return -1, fmt.Errorf("substring 'index' not found in %q", device)
	}

	start := idx + len("index")
	dev := device[start:]

	return strconv.Atoi(dev)
}

func HasVirtualDevice(deviceClass DeviceClass, devices []string) bool {
	virtualDevicesMapping := map[DeviceClass][]string{
		CameraClass: {"remote_camera_0"},
	}

	for _, v := range virtualDevicesMapping[deviceClass] {
		for _, d := range devices {
			if v == d {
				return true
			}
		}
	}
	return false
}

// AudioSink represents a PipeWire audio sink or source.
type AudioSink struct {
	ListIndex   int
	PipeWireID  int
	DisplayName string
	IsDefault   bool
	Processes   []string // processes currently using this sink/source
}

var nodeNameRE = regexp.MustCompile(`node\.name\s*=\s*"([^"]+)"`)

// AudioDevices holds all PipeWire audio sinks and sources found on the host.
type AudioDevices struct {
	Sinks   []AudioSink
	Sources []AudioSink
}

// GetAudioDevices returns all PipeWire audio sinks and sources, including which
// processes are currently using each one.
func GetAudioDevices() (AudioDevices, error) {
	out, err := exec.Command("wpctl", "status").Output()
	if err != nil {
		return AudioDevices{}, fmt.Errorf("unable to run wpctl status: %w", err)
	}
	s := string(out)
	sinks := parseAudioSubsection(s, "Sinks")
	sources := parseAudioSubsection(s, "Sources")

	processesByNode, err := getProcessesByNode()
	if err != nil {
		slog.Warn("unable to get audio process info", slog.String("error", err.Error()))
	} else {
		for i := range sinks {
			sinks[i].Processes = processesByNode[sinks[i].PipeWireID]
		}
		for i := range sources {
			sources[i].Processes = processesByNode[sources[i].PipeWireID]
		}
	}

	return AudioDevices{Sinks: sinks, Sources: sources}, nil
}

// getProcessesByNode uses pw-dump to build a map from sink/source node ID to
// the list of application names currently streaming to/from that node.
//
// PipeWire link direction:
//   - Sink consumers:   Stream/Output/Audio node → link(output=stream, input=sink)
//   - Source consumers: link(output=source, input=stream) ← Stream/Input/Audio node
func getProcessesByNode() (map[int][]string, error) {
	out, err := exec.Command("pw-dump").Output()
	if err != nil {
		return nil, fmt.Errorf("unable to run pw-dump: %w", err)
	}

	var objects []struct {
		ID   int             `json:"id"`
		Type string          `json:"type"`
		Info json.RawMessage `json:"info"`
	}
	if err := json.Unmarshal(out, &objects); err != nil {
		return nil, fmt.Errorf("unable to parse pw-dump output: %w", err)
	}

	type nodeInfo struct {
		Props map[string]any `json:"props"`
	}
	type linkInfo struct {
		OutputNodeID int `json:"output-node-id"`
		InputNodeID  int `json:"input-node-id"`
	}

	// stream node ID → application name
	outputStreams := map[int]string{} // Stream/Output/Audio (playing to a sink)
	inputStreams := map[int]string{}  // Stream/Input/Audio  (recording from a source)

	type link struct{ out, in int }
	var links []link

	for _, obj := range objects {
		switch obj.Type {
		case "PipeWire:Interface:Node":
			var info nodeInfo
			if err := json.Unmarshal(obj.Info, &info); err != nil {
				continue
			}
			mediaClass, _ := info.Props["media.class"].(string)
			if mediaClass != "Stream/Output/Audio" && mediaClass != "Stream/Input/Audio" {
				continue
			}
			appName, _ := info.Props["application.name"].(string)
			if appName == "" {
				appName, _ = info.Props["node.name"].(string)
			}
			if appName == "" {
				continue
			}
			if mediaClass == "Stream/Output/Audio" {
				outputStreams[obj.ID] = appName
			} else {
				inputStreams[obj.ID] = appName
			}

		case "PipeWire:Interface:Link":
			var info linkInfo
			if err := json.Unmarshal(obj.Info, &info); err != nil {
				continue
			}
			links = append(links, link{out: info.OutputNodeID, in: info.InputNodeID})
		}
	}

	result := map[int][]string{}
	for _, l := range links {
		// Sink consumer: stream(out) → sink(in)
		if appName, ok := outputStreams[l.out]; ok {
			result[l.in] = appendUnique(result[l.in], appName)
		}
		// Source consumer: source(out) → stream(in)
		if appName, ok := inputStreams[l.in]; ok {
			result[l.out] = appendUnique(result[l.out], appName)
		}
	}
	return result, nil
}

func appendUnique(slice []string, s string) []string {
	for _, v := range slice {
		if v == s {
			return slice
		}
	}
	return append(slice, s)
}

// GetAudioSinks returns all PipeWire audio sinks by parsing `wpctl status`.
func GetAudioSinks() ([]AudioSink, error) {
	d, err := GetAudioDevices()
	return d.Sinks, err
}

func parseAudioSubsection(output, subsection string) []AudioSink {
	var results []AudioSink
	inAudio := false
	inSubsection := false
	listIdx := 0

	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)

		if trimmed == "Audio" {
			inAudio, inSubsection = true, false
			continue
		}
		if trimmed == "Video" || trimmed == "Settings" {
			inAudio, inSubsection = false, false
			continue
		}
		if !inAudio {
			continue
		}
		if strings.Contains(line, subsection+":") {
			inSubsection = true
			continue
		}
		if strings.ContainsAny(line, "├└") {
			inSubsection = false
			continue
		}
		if !inSubsection {
			continue
		}

		clean := strings.TrimLeft(line, " │")
		if clean == "" {
			continue
		}
		isDefault := strings.HasPrefix(clean, "*")
		clean = strings.TrimLeft(clean, "* ")

		dotIdx := strings.Index(clean, ".")
		if dotIdx <= 0 {
			continue
		}
		id, err := strconv.Atoi(strings.TrimSpace(clean[:dotIdx]))
		if err != nil {
			continue
		}
		rest := strings.TrimSpace(clean[dotIdx+1:])
		if bracketIdx := strings.Index(rest, "["); bracketIdx >= 0 {
			rest = strings.TrimSpace(rest[:bracketIdx])
		}
		if rest == "" {
			continue
		}

		results = append(results, AudioSink{
			ListIndex:   listIdx,
			PipeWireID:  id,
			DisplayName: rest,
			IsDefault:   isDefault,
		})
		listIdx++
	}
	return results
}

// GetAudioSinkNodeName returns the stable PipeWire node name for a given sink ID
// by running `wpctl inspect <id>` and parsing the node.name property.
func GetAudioSinkNodeName(pipewireID int) (string, bool) {
	out, err := exec.Command("wpctl", "inspect", strconv.Itoa(pipewireID)).Output()
	if err != nil {
		slog.Warn("unable to run wpctl inspect", slog.Int("id", pipewireID), slog.String("error", err.Error()))
		return "", false
	}
	if m := nodeNameRE.FindSubmatch(out); m != nil {
		return string(m[1]), true
	}
	slog.Warn("node.name not found in wpctl inspect output", slog.Int("id", pipewireID))
	return "", false
}

func HasCSICamera(carriers []linuxconfig.Carrier) bool {
	for _, c := range carriers {
		if c.CarrierName != "media-carrier" {
			continue
		}
		if slices.ContainsFunc(c.EnabledDevices(), func(d linuxconfig.Device) bool { return strings.Contains(d.Device, "camera") }) {
			return true
		}

	}
	return false
}
