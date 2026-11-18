package collect

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

func missing(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "n/a", "[n/a]", "[not supported]", "[not available]", "[insufficient permissions]":
		return true
	default:
		return false
	}
}

func number(s string) (*float64, error) {
	s = strings.TrimSpace(s)
	if missing(s) {
		return nil, nil
	}
	value, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return nil, errors.New("invalid numeric metric")
	}
	return &value, nil
}

func parseSMI(data []byte) ([]GPU, error) {
	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = 7
	r.TrimLeadingSpace = true
	devices := []GPU{}
	for {
		row, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("invalid nvidia-smi CSV: %w", err)
		}
		index, err := strconv.Atoi(strings.TrimSpace(row[0]))
		if err != nil || index < 0 {
			return nil, errors.New("invalid nvidia-smi GPU index")
		}
		device := GPU{Index: index, Name: strings.TrimSpace(row[1]), UUID: strings.TrimSpace(row[2])}
		if missing(device.UUID) {
			device.UUID = ""
		}
		for i, target := range []**float64{&device.UtilizationPct, &device.MemoryUsedMiB, &device.MemoryTotalMiB, &device.TemperatureC} {
			*target, err = number(row[i+3])
			if err != nil {
				return nil, err
			}
		}
		devices = append(devices, device)
	}
	if len(devices) == 0 {
		return nil, errors.New("empty nvidia-smi output")
	}
	return devices, nil
}

// nvtop 3.3 snapshots return percentage and temperature strings and byte
// counts encoded as strings. Older snapshots omit the memory fields.
func parseNvtop(data []byte) ([]GPU, error) {
	if strings.TrimSpace(string(data)) == "No GPU to monitor." {
		return []GPU{}, nil
	}
	var rows []struct {
		Name        json.RawMessage `json:"device_name"`
		Utilization string          `json:"gpu_util"`
		MemoryUsed  json.Number     `json:"mem_used"`
		MemoryTotal json.Number     `json:"mem_total"`
		Temperature string          `json:"temp"`
	}
	if err := json.Unmarshal(data, &rows); err != nil || rows == nil {
		return nil, errors.New("invalid nvtop snapshot")
	}
	devices := make([]GPU, 0, len(rows))
	for index, row := range rows {
		d := GPU{Index: index}
		if len(row.Name) == 0 || json.Unmarshal(row.Name, &d.Name) != nil {
			return nil, errors.New("invalid nvtop device record")
		}
		var err error
		if d.UtilizationPct, err = number(strings.TrimSuffix(row.Utilization, "%")); err != nil {
			return nil, err
		}
		if d.MemoryUsedMiB, err = bytesToMiB(row.MemoryUsed); err != nil {
			return nil, err
		}
		if d.MemoryTotalMiB, err = bytesToMiB(row.MemoryTotal); err != nil {
			return nil, err
		}
		if d.TemperatureC, err = temperature(row.Temperature); err != nil {
			return nil, err
		}
		devices = append(devices, d)
	}
	return devices, nil
}

func bytesToMiB(raw json.Number) (*float64, error) {
	value, err := number(raw.String())
	if value != nil {
		*value /= 1024 * 1024
	}
	return value, err
}

func temperature(raw string) (*float64, error) {
	raw = strings.TrimSpace(raw)
	value, err := number(strings.TrimSuffix(strings.TrimSuffix(raw, "C"), "F"))
	if value != nil && strings.HasSuffix(raw, "F") {
		*value = (*value - 32) * 5 / 9
	}
	return value, err
}
