# xe-exporter

A high-performance Prometheus and OpenTelemetry exporter for Intel Arc GPUs (Battlemage and Alchemist) using the modern Linux `xe` kernel driver.

## Dev environment

- Go 1.26+ required for building
- Root privileges needed to read metrics from debugfs
- Linux Kernel 6.8+ with xe driver enabled

## Build & test

- Build: `make build`
- Install: `sudo make install`
- Setup service: `sudo make setup-service`
- Run tests: `go test -v`

## Conventions

- Uses Go modules with OpenTelemetry SDK for metrics
- Supports both Prometheus endpoint (`/metrics`) and OTLP gRPC push
- Collects VRAM, frequency, power, temperature, fan, throttle metrics
- Auto-discovers hwmon sensors for temperature readings
- Optional xe PMU per-engine busyness metrics (requires `-enable-pmu` flag)
- Uses systemd service file for deployment

## Pitfalls

- Requires root privileges to access debugfs metrics
- On Linux 6.14 kernels, enabling PMU engine events prevents `gt-c6` idle residency which affects `xe_gpu_gt_usage_percent`
- The exporter must be built on a system with the xe kernel driver present