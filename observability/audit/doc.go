// Package audit adapts the runtime mutation-audit contract to an
// application-owned durable outbox.
//
// The package defines no storage schema. Writer implementations must use the
// transaction handle carried by the context passed to Writer.Append. Admit
// registers that append through runtime.RegisterOutbox, so an append failure
// prevents commit and a rollback removes the record with the application
// write. Hook is a separate best-effort integration for the complete runtime
// audit lifecycle and does not make a transactional durability claim.
package audit
