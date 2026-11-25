# xe-exporter
This has been tested on a B60 only!

![Grafana Dashboard](assets/usage.png)

A high-performance Prometheus and OpenTelemetry exporter for Intel Arc GPUs (Battlemage and Alchemist) using the modern Linux `xe` kernel driver.

## Features

- **OTel Native:** Built using the OpenTelemetry Go SDK for unified metrics.
- **Dual Export:** Supports both Prometheus (`/metrics` endpoint) and OTLP gRPC push.
- **Real-time Monitoring:** Collects VRAM, frequency (actual/requested/min/max limits), power & power limits, temperatures (auto-discovered via hwmon), fan speeds, frequency throttling status/reasons, GPU utilization, and optional `xe` PMU per-engine busyness.
- **Systemd Ready:** Includes a pre-configured service file for easy deployment.
- **Enhanced VRAM Discovery:** Supports Linux 7.0+ debugfs tile*/vram_mm VRAM discovery
- **Improved Sensor Discovery:** Auto-discovers hwmon temperature and power limit sensors
- **Multi-GPU Support:** Works with multiple GPUs in the system
- **Throttling Diagnostics:** Reports frequency throttling status and reasons

## Metrics

| Metric Name | Description | Labels |
| ----------- | ----------- | ------ |
| `xe_gpu_vram_total_bytes` | Total VRAM capacity | `card`, `vram` |
| `xe_gpu_vram_used_bytes` | Current VRAM allocation | `card`, `vram` |
| `xe_gpu_gt_usage_percent` | GPU GT utilization percent | `card`, `gt` |
| `xe_gpu_engine_busy_percent` | GPU engine busyness percent via `xe` PMU (requires `-enable-pmu`) | `card`, `gt`, `engine` |
| `xe_gpu_frequency_actual_mhz` | Actual clock speed | `card`, `gt` |
| `xe_gpu_frequency_requested_mhz` | Requested clock speed | `card`, `gt` |
| `xe_gpu_frequency_min_mhz` | Configured minimum frequency | `card`, `gt` |
| `xe_gpu_frequency_max_mhz` | Configured maximum frequency | `card`, `gt` |
| `xe_gpu_power_watts` | Power draw (non-blocking energy delta) | `card`, `type` |
| `xe_gpu_power_cap_watts` | Configured power cap limit (`power*_cap`) | `card`, `type` |
| `xe_gpu_power_crit_watts` | Critical power limit (`power*_crit`) | `card`, `type` |
| `xe_gpu_temperature_celsius` | Temperature sensor reading (auto-discovered `temp*_input` / `temp*_label`) | `card`, `sensor` |
| `xe_gpu_fan_rpm` | Fan speed | `card`, `fan` |
| `xe_gpu_throttle_status` | Frequency throttle status (0 = unthrottled, >0 = throttled) | `card`, `gt` |
| `xe_gpu_throttle_reason` | Frequency throttle reasons (flags) | `card`, `gt`, `reason` |

## Requirements

- **Linux Kernel 6.8+** with the `xe` driver enabled.
- **Intel Arc GPU** (e.g., B60, A770).
- **Go 1.26+** (for building from source).
- **Root Privileges:** Required to read metrics from `debugfs`.

## Installation

### 1. Build from source
```bash
make build
sudo make install
```

### 2. Setup as a Service
```bash
sudo make setup-service
```

## Configuration

The exporter is configured via `/etc/default/xe-exporter`.

To modify options like the Prometheus port or OTLP endpoint:

1. Edit `/etc/default/xe-exporter`:
```bash
# /etc/default/xe-exporter
XE_EXPORTER_OPTS="-prom-port 9101 -otlp-endpoint my-otel-collector:4317"
```

2. Restart the service:
```bash
sudo systemctl restart xe-exporter
```

The following flags are available:
- `-prom-port`: Port for Prometheus metrics (default: `9101`)
- `-enable-prom`: Enable/disable Prometheus endpoint (default: `true`)
- `-otlp-endpoint`: OTLP gRPC endpoint (e.g., `localhost:4317`)
- `-enable-pmu`: Enable `xe` PMU per-engine busyness metrics (`xe_gpu_engine_busy_percent`, default: `false`). Note: on Linux 6.14 `xe` kernels, holding PMU engine events open prevents `gt-c6` idle residency (causing `xe_gpu_gt_usage_percent` to report 100%).
- `-debug`: Enable verbose collection logging (default: `false`)

## Prometheus Configuration

To scrape metrics from this exporter, add the following job to your `prometheus.yml`:

```yaml
scrape_configs:
  - job_name: 'xe-exporter'
    static_configs:
      - targets: ['localhost:9101']
    scrape_interval: 2s
```

## OpenTelemetry Configuration (OTLP)

If you prefer pushing metrics to an OTel Collector, enable the OTLP flag in the configuration file:

```bash
# /etc/default/xe-exporter
XE_EXPORTER_OPTS="-otlp-endpoint my-otel-collector:4317"
```

## License
MIT
