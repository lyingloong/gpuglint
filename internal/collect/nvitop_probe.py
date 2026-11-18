# Executed via SSH on stdin. The only third-party dependency is the target's
# existing nvitop installation. JSON numbers use the same units as the CLI.
import json
import math
import sys

try:
    from nvitop import Device, libnvml
except ImportError as exc:
    sys.stderr.write(str(exc) + "\n")
    sys.exit(127)


def metric(value, scale=1):
    if not isinstance(value, (float, int)) or not math.isfinite(value):
        return None
    return value / scale


def text(value):
    return "" if value is None or str(value) == "N/A" else str(value)


devices = []
# Ask NVML to raise on initialization errors. A driver failure must not
# become an empty, successful list via Device.count()'s default of zero.
count = libnvml.nvmlQuery("nvmlDeviceGetCount", ignore_errors=False)
for index in range(count):
    device = Device(index)
    with device.oneshot():
        devices.append({
            "index": index,
            "uuid": text(device.uuid()),
            "name": text(device.name()),
            "utilization_pct": metric(device.gpu_utilization()),
            "memory_total_mib": metric(device.memory_total(), 1048576),
            "memory_used_mib": metric(device.memory_used(), 1048576),
            "temperature_c": metric(device.temperature()),
        })
print(json.dumps(devices, allow_nan=False))
