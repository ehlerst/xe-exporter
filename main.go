package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.17.0"
	"golang.org/x/sys/unix"
)

type GTStats struct {
	LastIdleRes uint64
	LastTime    time.Time
}

type EnergyStats struct {
	LastEnergy  uint64
	LastEnergyT time.Time
}

type pmuFormat struct {
	pmuType       uint32
	activeEvent   uint64
	totalEvent    uint64
	eventShift    uint
	instanceShift uint
	classShift    uint
	gtShift       uint
}

type EnginePMU struct {
	GTID       string
	EngineName string
	ActiveFD   int
	TotalFD    int
	Grouped    bool
	LastActive uint64
	LastTotal  uint64
	HasLast    bool
}

type VRAMSource struct {
	ID   string
	Path string
}

type GPU struct {
	ID        string
	SysPath   string
	DebugPath string
	HwmonPath string
	GTs       map[string]*GTStats
	VRAMs     []VRAMSource
	Energy    map[string]*EnergyStats
	Engines   []*EnginePMU
	mu        sync.Mutex
}

var engineClassIDs = map[string]uint64{
	"rcs":  0,
	"bcs":  1,
	"vcs":  2,
	"vecs": 3,
	"ccs":  4,
}

func isDRMCardDir(name string) bool {
	if !strings.HasPrefix(name, "card") {
		return false
	}
	suffix := strings.TrimPrefix(name, "card")
	if suffix == "" {
		return false
	}
	for i := 0; i < len(suffix); i++ {
		if suffix[i] < '0' || suffix[i] > '9' {
			return false
		}
	}
	return true
}

func discoverVRAMs(debugPath string) []VRAMSource {
	if debugPath == "" {
		return nil
	}

	seen := make(map[string]bool)
	var sources []VRAMSource

	// Linux 7.0+ xe driver: /sys/kernel/debug/dri/<card>/tile<N>/vram_mm -> vram="vram<N>_mm"
	tilePaths, _ := filepath.Glob(filepath.Join(debugPath, "tile*", "vram_mm"))
	sort.Strings(tilePaths)
	for _, p := range tilePaths {
		tileDir := filepath.Base(filepath.Dir(p))
		suffix := strings.TrimPrefix(tileDir, "tile")
		if suffix == "" || suffix == tileDir {
			continue
		}
		vramID := fmt.Sprintf("vram%s_mm", suffix)
		if !seen[vramID] {
			seen[vramID] = true
			sources = append(sources, VRAMSource{ID: vramID, Path: p})
		}
	}

	// Legacy (Linux <= 6.x) xe driver: /sys/kernel/debug/dri/<card>/vram<N>_mm
	legacyPaths, _ := filepath.Glob(filepath.Join(debugPath, "vram*_mm"))
	sort.Strings(legacyPaths)
	for _, p := range legacyPaths {
		vramID := filepath.Base(p)
		if !seen[vramID] {
			seen[vramID] = true
			sources = append(sources, VRAMSource{ID: vramID, Path: p})
		}
	}

	sort.Slice(sources, func(i, j int) bool {
		return sources[i].ID < sources[j].ID
	})
	return sources
}

func discoverGPUs(enablePMU bool) []*GPU {
	var gpus []*GPU
	cards, _ := filepath.Glob("/sys/class/drm/card[0-9]*")
	sort.Strings(cards)
	for _, cardPath := range cards {
		cardID := filepath.Base(cardPath)
		if !isDRMCardDir(cardID) {
			continue
		}

		// Only monitor devices bound to the Intel xe driver when driver info is available.
		if driverLink, err := filepath.EvalSymlinks(filepath.Join(cardPath, "device/driver")); err == nil {
			if filepath.Base(driverLink) != "xe" {
				continue
			}
		}

		gpu := &GPU{
			ID:      cardID,
			SysPath: cardPath,
			GTs:     make(map[string]*GTStats),
			Energy:  make(map[string]*EnergyStats),
		}

		// Discover DebugPath
		// card0 -> /sys/kernel/debug/dri/0
		// card1 -> /sys/kernel/debug/dri/1
		idStr := strings.TrimPrefix(cardID, "card")
		debugPath := fmt.Sprintf("/sys/kernel/debug/dri/%s", idStr)
		if _, err := os.Stat(debugPath); err == nil {
			gpu.DebugPath = debugPath
		}

		// Discover VRAMs in debugfs (Linux 7.0+ tile*/vram_mm and legacy vram*_mm)
		if gpu.DebugPath != "" {
			gpu.VRAMs = discoverVRAMs(gpu.DebugPath)
		}

		// Discover GTs in sysfs
		// /sys/class/drm/cardX/device/tileX/gtX
		gtPaths, _ := filepath.Glob(filepath.Join(cardPath, "device/tile*/gt*"))
		sort.Strings(gtPaths)
		for _, gtp := range gtPaths {
			// Extract tileX/gtX
			parts := strings.Split(gtp, "/")
			if len(parts) >= 2 {
				gtID := parts[len(parts)-2] + "/" + parts[len(parts)-1]
				gpu.GTs[gtID] = &GTStats{}
			}
		}

		// Discover HWMON
		hwmonPaths, _ := filepath.Glob(filepath.Join(cardPath, "device/hwmon/hwmon*"))
		sort.Strings(hwmonPaths)
		if len(hwmonPaths) > 0 {
			gpu.HwmonPath = hwmonPaths[0]
		}

		// Optionally discover xe PMU engine busyness counters
		if enablePMU {
			gpu.Engines = discoverPMUEngines(gpu)
		}

		gpus = append(gpus, gpu)
	}
	return gpus
}

func parsePMUConfigShift(raw string) (uint, error) {
	s := strings.TrimSpace(raw)
	if !strings.HasPrefix(s, "config:") {
		return 0, fmt.Errorf("unexpected pmu format: %q", s)
	}
	rangeStr := strings.TrimPrefix(s, "config:")
	if idx := strings.Index(rangeStr, "-"); idx != -1 {
		rangeStr = rangeStr[:idx]
	}
	shift, err := strconv.ParseUint(rangeStr, 10, 64)
	if err != nil {
		return 0, err
	}
	return uint(shift), nil
}

func parsePMUEventID(raw string) (uint64, error) {
	for _, field := range strings.Split(strings.TrimSpace(raw), ",") {
		field = strings.TrimSpace(field)
		if strings.HasPrefix(field, "event=") {
			return strconv.ParseUint(strings.TrimPrefix(field, "event="), 0, 64)
		}
	}
	return 0, fmt.Errorf("no event= field in %q", raw)
}

func loadPMUFormat(pmuDir string) (*pmuFormat, error) {
	typeVal, ok := readUint64E(filepath.Join(pmuDir, "type"))
	if !ok {
		return nil, fmt.Errorf("failed to read PMU type in %s", pmuDir)
	}

	activeBytes, err := os.ReadFile(filepath.Join(pmuDir, "events/engine-active-ticks"))
	if err != nil {
		return nil, err
	}
	activeEvent, err := parsePMUEventID(string(activeBytes))
	if err != nil {
		return nil, err
	}

	totalBytes, err := os.ReadFile(filepath.Join(pmuDir, "events/engine-total-ticks"))
	if err != nil {
		return nil, err
	}
	totalEvent, err := parsePMUEventID(string(totalBytes))
	if err != nil {
		return nil, err
	}

	readShift := func(name string) (uint, error) {
		b, err := os.ReadFile(filepath.Join(pmuDir, "format", name))
		if err != nil {
			return 0, err
		}
		return parsePMUConfigShift(string(b))
	}

	eventShift, err := readShift("event")
	if err != nil {
		return nil, err
	}
	instanceShift, err := readShift("engine_instance")
	if err != nil {
		return nil, err
	}
	classShift, err := readShift("engine_class")
	if err != nil {
		return nil, err
	}
	gtShift, err := readShift("gt")
	if err != nil {
		return nil, err
	}

	return &pmuFormat{
		pmuType:       uint32(typeVal),
		activeEvent:   activeEvent,
		totalEvent:    totalEvent,
		eventShift:    eventShift,
		instanceShift: instanceShift,
		classShift:    classShift,
		gtShift:       gtShift,
	}, nil
}

func (f *pmuFormat) config(eventID, gtNum, classID, instance uint64) uint64 {
	return (eventID << f.eventShift) |
		(instance << f.instanceShift) |
		(classID << f.classShift) |
		(gtNum << f.gtShift)
}

func discoverPMUEngines(gpu *GPU) []*EnginePMU {
	devPath, err := filepath.EvalSymlinks(filepath.Join(gpu.SysPath, "device"))
	if err != nil {
		return nil
	}
	pciAddr := filepath.Base(devPath)
	pmuName := "xe_" + strings.ReplaceAll(pciAddr, ":", "_")
	pmuDir := filepath.Join("/sys/bus/event_source/devices", pmuName)

	fmtInfo, err := loadPMUFormat(pmuDir)
	if err != nil {
		log.Printf("PMU unavailable for %s (%s): %v", gpu.ID, pmuName, err)
		return nil
	}

	var gtIDs []string
	for gtID := range gpu.GTs {
		gtIDs = append(gtIDs, gtID)
	}
	sort.Strings(gtIDs)

	var engines []*EnginePMU
	for _, gtID := range gtIDs {
		gtBase := filepath.Base(gtID)
		if !strings.HasPrefix(gtBase, "gt") {
			continue
		}
		gtNum, err := strconv.ParseUint(strings.TrimPrefix(gtBase, "gt"), 10, 64)
		if err != nil {
			continue
		}

		engineDirs, _ := filepath.Glob(filepath.Join(gpu.SysPath, "device", gtID, "engines/*"))
		sort.Strings(engineDirs)
		for _, ed := range engineDirs {
			className := filepath.Base(ed)
			classID, ok := engineClassIDs[className]
			if !ok {
				continue
			}

			for inst := uint64(0); inst < 16; inst++ {
				eng, err := openEnginePMU(fmtInfo, gtID, gtNum, className, classID, inst)
				if err != nil {
					break
				}
				engines = append(engines, eng)
			}
		}
	}

	log.Printf("Opened %d xe PMU engine counters for %s (%s)", len(engines), gpu.ID, pmuName)
	return engines
}

func openEnginePMU(f *pmuFormat, gtID string, gtNum uint64, className string, classID, inst uint64) (*EnginePMU, error) {
	activeAttr := unix.PerfEventAttr{
		Type:        f.pmuType,
		Size:        uint32(unsafe.Sizeof(unix.PerfEventAttr{})),
		Config:      f.config(f.activeEvent, gtNum, classID, inst),
		Read_format: unix.PERF_FORMAT_GROUP,
	}
	activeFD, err := unix.PerfEventOpen(&activeAttr, -1, 0, -1, unix.PERF_FLAG_FD_CLOEXEC)
	if err != nil {
		return nil, err
	}

	totalAttr := unix.PerfEventAttr{
		Type:        f.pmuType,
		Size:        uint32(unsafe.Sizeof(unix.PerfEventAttr{})),
		Config:      f.config(f.totalEvent, gtNum, classID, inst),
		Read_format: unix.PERF_FORMAT_GROUP,
	}
	totalFD, err := unix.PerfEventOpen(&totalAttr, -1, 0, activeFD, unix.PERF_FLAG_FD_CLOEXEC)
	if err == nil {
		return &EnginePMU{
			GTID:       gtID,
			EngineName: fmt.Sprintf("%s%d", className, inst),
			ActiveFD:   activeFD,
			TotalFD:    totalFD,
			Grouped:    true,
		}, nil
	}

	// Fallback to ungrouped counters if the kernel rejects grouping.
	unix.Close(activeFD)
	activeAttr.Read_format = 0
	activeFD, err = unix.PerfEventOpen(&activeAttr, -1, 0, -1, unix.PERF_FLAG_FD_CLOEXEC)
	if err != nil {
		return nil, err
	}
	totalAttr.Read_format = 0
	totalFD, err = unix.PerfEventOpen(&totalAttr, -1, 0, -1, unix.PERF_FLAG_FD_CLOEXEC)
	if err != nil {
		unix.Close(activeFD)
		return nil, err
	}

	return &EnginePMU{
		GTID:       gtID,
		EngineName: fmt.Sprintf("%s%d", className, inst),
		ActiveFD:   activeFD,
		TotalFD:    totalFD,
		Grouped:    false,
	}, nil
}

func (e *EnginePMU) readTicks() (active, total uint64, err error) {
	if e.Grouped {
		var buf [24]byte
		n, err := unix.Read(e.ActiveFD, buf[:])
		if err != nil {
			return 0, 0, err
		}
		if n < 24 {
			return 0, 0, fmt.Errorf("short perf group read: %d bytes", n)
		}
		nr := binary.LittleEndian.Uint64(buf[0:8])
		if nr < 2 {
			return 0, 0, fmt.Errorf("unexpected perf group count: %d", nr)
		}
		return binary.LittleEndian.Uint64(buf[8:16]), binary.LittleEndian.Uint64(buf[16:24]), nil
	}

	var b1, b2 [8]byte
	if n, err := unix.Read(e.ActiveFD, b1[:]); err != nil || n < 8 {
		return 0, 0, fmt.Errorf("active perf read failed: %v", err)
	}
	if n, err := unix.Read(e.TotalFD, b2[:]); err != nil || n < 8 {
		return 0, 0, fmt.Errorf("total perf read failed: %v", err)
	}
	return binary.LittleEndian.Uint64(b1[:]), binary.LittleEndian.Uint64(b2[:]), nil
}

func (e *EnginePMU) close() {
	if e.TotalFD >= 0 {
		_ = unix.Close(e.TotalFD)
		e.TotalFD = -1
	}
	if e.ActiveFD >= 0 {
		_ = unix.Close(e.ActiveFD)
		e.ActiveFD = -1
	}
}

func readUint64E(path string) (uint64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	val, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, false
	}
	return val, true
}

func readUint64(path string) uint64 {
	val, _ := readUint64E(path)
	return val
}

func readFloat64E(path string) (float64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	val, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
	if err != nil {
		return 0, false
	}
	return val, true
}

func hwmonSensorLabel(sensorPath, prefix, suffix string, keepPrefixFallback bool) string {
	base := filepath.Base(sensorPath)
	labelPath := strings.TrimSuffix(sensorPath, suffix) + "_label"
	if labelBytes, err := os.ReadFile(labelPath); err == nil {
		if label := strings.TrimSpace(string(labelBytes)); label != "" {
			return label
		}
	}
	fallback := strings.TrimSuffix(base, suffix)
	if !keepPrefixFallback {
		fallback = strings.TrimPrefix(fallback, prefix)
	}
	return fallback
}

func collectVRAM(debugFile string) (total, used uint64) {
	file, err := os.Open(debugFile)
	if err != nil {
		return 0, 0
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "size:") {
			parts := strings.Split(line, ":")
			if len(parts) == 2 {
				total, _ = strconv.ParseUint(strings.TrimSpace(parts[1]), 10, 64)
			}
		}
		if strings.HasPrefix(line, "usage:") {
			parts := strings.Split(line, ":")
			if len(parts) == 2 {
				used, _ = strconv.ParseUint(strings.TrimSpace(parts[1]), 10, 64)
			}
		}
	}
	return
}

type hwmonReading struct {
	label string
	value float64
}

func collectHwmonTemperatures(hwmonPath string) []hwmonReading {
	var readings []hwmonReading
	tempPaths, _ := filepath.Glob(filepath.Join(hwmonPath, "temp*_input"))
	sort.Strings(tempPaths)
	for _, tempPath := range tempPaths {
		milliC, ok := readFloat64E(tempPath)
		if !ok {
			continue
		}
		label := hwmonSensorLabel(tempPath, "temp", "_input", true)
		readings = append(readings, hwmonReading{
			label: label,
			value: milliC / 1000.0,
		})
	}
	return readings
}

func collectHwmonPowerLimits(hwmonPath, suffix string) []hwmonReading {
	var readings []hwmonReading
	paths, _ := filepath.Glob(filepath.Join(hwmonPath, "power*"+suffix))
	sort.Strings(paths)
	for _, p := range paths {
		microW, ok := readFloat64E(p)
		if !ok {
			continue
		}
		label := hwmonSensorLabel(p, "power", suffix, false)
		readings = append(readings, hwmonReading{
			label: label,
			value: microW / 1000000.0,
		})
	}
	return readings
}

func main() {
	promPort := flag.String("prom-port", "9101", "Prometheus metrics port")
	enableProm := flag.Bool("enable-prom", true, "Enable Prometheus /metrics endpoint")
	otlpEndpoint := flag.String("otlp-endpoint", "", "OTLP gRPC endpoint")
	enablePMU := flag.Bool("enable-pmu", false, "Enable xe PMU per-engine busyness metrics (note: blocks gt-c6 idle residency on Linux 6.14 xe driver)")
	debug := flag.Bool("debug", false, "Enable verbose collection logging")
	flag.Parse()

	ctx := context.Background()
	res, _ := resource.New(ctx, resource.WithAttributes(semconv.ServiceNameKey.String("xe-exporter")))

	var options []sdkmetric.Option
	options = append(options, sdkmetric.WithResource(res))

	if *enableProm {
		promExporter, _ := prometheus.New()
		options = append(options, sdkmetric.WithReader(promExporter))
	}

	if *otlpEndpoint != "" {
		otlpExporter, _ := otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithEndpoint(*otlpEndpoint), otlpmetricgrpc.WithInsecure())
		options = append(options, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(otlpExporter, sdkmetric.WithInterval(10*time.Second))))
	}

	provider := sdkmetric.NewMeterProvider(options...)
	defer provider.Shutdown(ctx)
	otel.SetMeterProvider(provider)

	meter := provider.Meter("xe-exporter")

	// Gauges
	vramUsedGauge, _ := meter.Float64ObservableGauge("xe_gpu_vram_used_bytes", metric.WithDescription("Used VRAM in bytes"))
	vramTotalGauge, _ := meter.Float64ObservableGauge("xe_gpu_vram_total_bytes", metric.WithDescription("Total VRAM in bytes"))
	gtUsageGauge, _ := meter.Float64ObservableGauge("xe_gpu_gt_usage_percent", metric.WithDescription("GPU GT utilization percent"))
	engineBusyGauge, _ := meter.Float64ObservableGauge("xe_gpu_engine_busy_percent", metric.WithDescription("GPU engine busyness percent from xe PMU"))
	freqActualGauge, _ := meter.Float64ObservableGauge("xe_gpu_frequency_actual_mhz", metric.WithDescription("Actual GPU frequency in MHz"))
	freqRequestedGauge, _ := meter.Float64ObservableGauge("xe_gpu_frequency_requested_mhz", metric.WithDescription("Requested GPU frequency in MHz"))
	freqMinGauge, _ := meter.Float64ObservableGauge("xe_gpu_frequency_min_mhz", metric.WithDescription("Configured minimum GPU frequency in MHz"))
	freqMaxGauge, _ := meter.Float64ObservableGauge("xe_gpu_frequency_max_mhz", metric.WithDescription("Configured maximum GPU frequency in MHz"))
	throttleStatusGauge, _ := meter.Float64ObservableGauge("xe_gpu_throttle_status", metric.WithDescription("GPU frequency throttle status (0 = unthrottled, >0 = throttled)"))
	throttleReasonGauge, _ := meter.Float64ObservableGauge("xe_gpu_throttle_reason", metric.WithDescription("GPU frequency throttle reasons"))
	powerGauge, _ := meter.Float64ObservableGauge("xe_gpu_power_watts", metric.WithDescription("Approximate GPU power consumption in Watts"))
	powerCapGauge, _ := meter.Float64ObservableGauge("xe_gpu_power_cap_watts", metric.WithDescription("GPU power cap limit in Watts"))
	powerCritGauge, _ := meter.Float64ObservableGauge("xe_gpu_power_crit_watts", metric.WithDescription("GPU critical power limit in Watts"))
	tempGauge, _ := meter.Float64ObservableGauge("xe_gpu_temperature_celsius", metric.WithDescription("GPU temperature in degrees Celsius"))
	fanGauge, _ := meter.Float64ObservableGauge("xe_gpu_fan_rpm", metric.WithDescription("GPU Fan Speed in RPM"))

	gpus := discoverGPUs(*enablePMU)
	defer func() {
		for _, gpu := range gpus {
			for _, eng := range gpu.Engines {
				eng.close()
			}
		}
	}()
	log.Printf("Discovered %d GPUs (pmu=%v)", len(gpus), *enablePMU)

	_, _ = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		start := time.Now()
		if *debug {
			log.Println("Metric collection started...")
		}
		for _, gpu := range gpus {
			gpu.mu.Lock()
			cardAttr := attribute.String("card", gpu.ID)

			// VRAM
			for _, vram := range gpu.VRAMs {
				total, used := collectVRAM(vram.Path)
				vramAttr := attribute.NewSet(cardAttr, attribute.String("vram", vram.ID))
				o.ObserveFloat64(vramTotalGauge, float64(total), metric.WithAttributeSet(vramAttr))
				o.ObserveFloat64(vramUsedGauge, float64(used), metric.WithAttributeSet(vramAttr))
			}

			// GTs and Frequencies
			for gtID, stats := range gpu.GTs {
				gtPath := filepath.Join(gpu.SysPath, "device", gtID)
				gtAttr := attribute.NewSet(cardAttr, attribute.String("gt", gtID))

				// Frequency
				actual := readUint64(filepath.Join(gtPath, "freq0/act_freq"))
				requested := readUint64(filepath.Join(gtPath, "freq0/cur_freq"))
				o.ObserveFloat64(freqActualGauge, float64(actual), metric.WithAttributeSet(gtAttr))
				o.ObserveFloat64(freqRequestedGauge, float64(requested), metric.WithAttributeSet(gtAttr))

				minFreq := readUint64(filepath.Join(gtPath, "freq0/min_freq"))
				maxFreq := readUint64(filepath.Join(gtPath, "freq0/max_freq"))
				o.ObserveFloat64(freqMinGauge, float64(minFreq), metric.WithAttributeSet(gtAttr))
				o.ObserveFloat64(freqMaxGauge, float64(maxFreq), metric.WithAttributeSet(gtAttr))

				// Throttle Status and Reasons
				throttlePath := filepath.Join(gtPath, "freq0/throttle")
				if _, err := os.Stat(throttlePath); err == nil {
					statusVal := readUint64(filepath.Join(throttlePath, "status"))
					o.ObserveFloat64(throttleStatusGauge, float64(statusVal), metric.WithAttributeSet(gtAttr))

					reasons, _ := filepath.Glob(filepath.Join(throttlePath, "reason_*"))
					for _, reasonPath := range reasons {
						reasonName := strings.TrimPrefix(filepath.Base(reasonPath), "reason_")
						reasonVal := readUint64(reasonPath)
						reasonAttr := attribute.NewSet(cardAttr, attribute.String("gt", gtID), attribute.String("reason", reasonName))
						o.ObserveFloat64(throttleReasonGauge, float64(reasonVal), metric.WithAttributeSet(reasonAttr))
					}
				}

				// Usage
				idlePath := filepath.Join(gtPath, "gtidle/idle_residency_ms")
				if currentIdle, ok := readUint64E(idlePath); ok {
					now := time.Now()
					if !stats.LastTime.IsZero() && currentIdle >= stats.LastIdleRes {
						dt := float64(now.Sub(stats.LastTime).Milliseconds())
						didle := float64(currentIdle - stats.LastIdleRes)
						if dt > 0 {
							usage := 100.0 * (1.0 - (didle / dt))
							if usage < 0 {
								usage = 0
							}
							if usage > 100 {
								usage = 100
							}
							o.ObserveFloat64(gtUsageGauge, usage, metric.WithAttributeSet(gtAttr))
						}
					}
					stats.LastIdleRes = currentIdle
					stats.LastTime = now
				}
			}

			// Optional xe PMU per-engine busyness
			for _, eng := range gpu.Engines {
				active, total, err := eng.readTicks()
				if err != nil {
					if *debug {
						log.Printf("PMU read error (%s %s %s): %v", gpu.ID, eng.GTID, eng.EngineName, err)
					}
					continue
				}
				if eng.HasLast && total > eng.LastTotal && active >= eng.LastActive {
					busy := 100.0 * float64(active-eng.LastActive) / float64(total-eng.LastTotal)
					if busy < 0 {
						busy = 0
					}
					if busy > 100 {
						busy = 100
					}
					engAttr := attribute.NewSet(cardAttr, attribute.String("gt", eng.GTID), attribute.String("engine", eng.EngineName))
					o.ObserveFloat64(engineBusyGauge, busy, metric.WithAttributeSet(engAttr))
				}
				eng.LastActive = active
				eng.LastTotal = total
				eng.HasLast = true
			}

			// HWMON (Power, Power Limits, Temperatures, and Fans)
			if gpu.HwmonPath != "" {
				// Non-blocking Power (energy counter delta between scrapes)
				energyPaths, _ := filepath.Glob(filepath.Join(gpu.HwmonPath, "energy*_input"))
				sort.Strings(energyPaths)
				for _, energyPath := range energyPaths {
					energyID := filepath.Base(energyPath)
					labelVal := hwmonSensorLabel(energyPath, "energy", "_input", false)

					eVal, ok := readUint64E(energyPath)
					if !ok {
						continue
					}
					now := time.Now()

					stats, exists := gpu.Energy[energyID]
					if !exists {
						stats = &EnergyStats{}
						gpu.Energy[energyID] = stats
					}

					if !stats.LastEnergyT.IsZero() && eVal >= stats.LastEnergy {
						dt := now.Sub(stats.LastEnergyT).Seconds()
						de := float64(eVal - stats.LastEnergy)
						if dt > 0 {
							power := (de / 1000000.0) / dt
							o.ObserveFloat64(powerGauge, power, metric.WithAttributes(cardAttr, attribute.String("type", labelVal)))
						}
					}
					stats.LastEnergy = eVal
					stats.LastEnergyT = now
				}

				// Power Limits (power*_cap and power*_crit in microwatts)
				for _, r := range collectHwmonPowerLimits(gpu.HwmonPath, "_cap") {
					o.ObserveFloat64(powerCapGauge, r.value, metric.WithAttributes(cardAttr, attribute.String("type", r.label)))
				}
				for _, r := range collectHwmonPowerLimits(gpu.HwmonPath, "_crit") {
					o.ObserveFloat64(powerCritGauge, r.value, metric.WithAttributes(cardAttr, attribute.String("type", r.label)))
				}

				// Auto-discovered Temperatures (temp*_input in millidegrees Celsius)
				for _, r := range collectHwmonTemperatures(gpu.HwmonPath) {
					o.ObserveFloat64(tempGauge, r.value, metric.WithAttributes(cardAttr, attribute.String("sensor", r.label)))
				}

				// Fans
				fans, _ := filepath.Glob(filepath.Join(gpu.HwmonPath, "fan*_input"))
				sort.Strings(fans)
				for _, fanPath := range fans {
					fanID := filepath.Base(fanPath)
					if rpm, ok := readUint64E(fanPath); ok {
						o.ObserveFloat64(fanGauge, float64(rpm), metric.WithAttributes(cardAttr, attribute.String("fan", fanID)))
					}
				}
			}
			gpu.mu.Unlock()
		}
		if *debug {
			log.Printf("Metric collection completed in %s", time.Since(start))
		}
		return nil
	}, vramTotalGauge, vramUsedGauge, gtUsageGauge, engineBusyGauge, freqActualGauge, freqRequestedGauge, freqMinGauge, freqMaxGauge, throttleStatusGauge, throttleReasonGauge, powerGauge, powerCapGauge, powerCritGauge, tempGauge, fanGauge)

	if *enableProm {
		go func() {
			http.Handle("/metrics", promhttp.Handler())
			log.Printf("Prometheus metrics at :%s/metrics", *promPort)
			if err := http.ListenAndServe(":"+*promPort, nil); err != nil {
				log.Fatal(err)
			}
		}()
	}

	select {}
}
