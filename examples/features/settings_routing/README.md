# Settings And Routing

This example shows `ModelSettings`, mode-level model routing, role-specific overrides, reasoning levels, and text verbosity.

Run it:

```sh
go test ./examples/features/settings_routing
```

How to use this feature:

- Put model behavior in `agentsdk.ModelSettings`.
- Use `ModeReasoningSettings`, `ModeTextVerbositySettings`, or `ModeRoutingSettings` to convert mode labels into provider settings.
- Use `ResolveModeRouting` for a top-level agent model.
- Use `ResolveRoleModeRouting` for specialist or sub-agent roles.

Runnable source: [settings_routing_test.go](settings_routing_test.go).
