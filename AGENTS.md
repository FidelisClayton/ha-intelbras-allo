# Development rules

This project is in development. Do not claim an installable integration or publish installation commands until the complete release has been validated on a clean supported Home Assistant installation and real hardware.

- Keep the protocol core in Go and the Home Assistant adapter thin and in Python. The first runtime target is Linux amd64.
- Preserve trust validation: approved cloud destination allowlist, server-chain validation, exact approved leaf-certificate pins and rejection of redirects. Never add an insecure mode, automatic regional fallback or unapproved endpoints. Validate identities and untrusted inputs before use.
- The only approved captured-certificate distribution exception is `internal/bootstrap/assets/qualvision-ca.pem`, the public Qualvision CA with certificate DER SHA-256 `cbc77b01d58308cc0fa8ee4af2e508af2cb3864e5a88628b0e7d0e474f4d7763`. This exception includes no private key or client certificate; all other restrictions below remain in force.
- Never put credentials, tokens, private device identifiers, private keys, captured certificates, APKs or extracted assets, packet/media captures, histories or private native-client profiles in source, releases, logs or diagnostics. Use synthetic fixtures. Review release contents explicitly; `.gitignore` is not a security boundary.
- Each installation must generate and persist its own receiver identity. Never copy, replace or unregister a phone, emulator or existing production bridge identity.
- Workers run offline checks only. The parent owns explicitly authorized live calls and physical tests. Do not automatically retry authentication or registration. Preserve existing production services and private state.
- Do not implement unlocking, relays, QR provisioning, firmware/settings changes, or notification/light automations.
- Preserve applicable third-party notices when adapting code. Do not copy protocol code without reviewing its license and carrying the required notices.
- Keep the first release scope complete: physical bell events, live video, device microphone audio and talk from the current browser/iPhone microphone. Prerecorded audio does not validate live talk. Stop and report unresolved bootstrap, trust, packaging or microphone-transport decisions rather than weakening the setup contract.
- When implementation modules exist, add or update meaningful offline tests for changed behavior and run their local test, race/static-analysis and build checks as applicable. Document the exact commands with those modules. Never substitute synthetic checks for physical-device or clean-install evidence. Documentation-only changes require manual review of accuracy, scope and links.
- Do not commit, push, publish or start servers unless the parent explicitly assigns that action.
