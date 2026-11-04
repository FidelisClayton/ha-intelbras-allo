"""Allowlisted diagnostics contain no account, device IDs, credentials or SDP."""


async def async_get_config_entry_diagnostics(hass, entry):
    return {
        "available": getattr(getattr(entry, "runtime_data", None), "available", False),
        "platform": "linux-amd64",
        "integration_version": "0.1.0",
    }
