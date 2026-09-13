// Package reflectadapter provides an opt-in startup adapter from explicitly
// tagged Go values to the Naatre runtime registry.
//
// Explicit runtime.Bind, runtime.BindField, and runtime.BindCall registration
// remains the production recommendation. Reflection is compiled only during
// startup: a Compiled value contains ordinary runtime.Definition values and
// invocation closures, so requests never trigger member discovery or mutate
// reflection metadata.
//
// Only exported data or function fields carrying a non-empty `naatre` tag and
// exported methods named in Options.Allowlist are reachable. All definitions
// still pass through runtime.Registry.Register and therefore retain the core
// authorization, resource, cost, scheduling, and output-completion checks.
package reflectadapter
