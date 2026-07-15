# Sandbox

This example runs a command through the SDK executor API. The executor sanitizes the child environment and bounds execution time and output; deploy the agent in an external sandbox when running untrusted commands.

Run it:

```sh
go test ./examples/features/sandbox
```
