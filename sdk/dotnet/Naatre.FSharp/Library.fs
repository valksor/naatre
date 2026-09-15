namespace Valksor.Naatre.FSharp

open System
open System.Collections.Generic
open System.Threading
open Valksor.Naatre
open Valksor.Naatre.Client

[<RequireQualifiedAccess>]
type Presence<'T> =
    | Missing
    | Null
    | Failed of IReadOnlyList<NaatreError>
    | Skipped of reason: string
    | Pending
    | Present of value: 'T

[<RequireQualifiedAccess>]
module Presence =
    let ofCore (value: Valksor.Naatre.Presence<'T>) : Presence<'T> =
        match value.State with
        | PresenceState.Missing -> Presence.Missing
        | PresenceState.Null -> Presence.Null
        | PresenceState.Failed -> Presence.Failed value.Errors
        | PresenceState.Skipped -> Presence.Skipped value.Reason
        | PresenceState.Pending -> Presence.Pending
        | PresenceState.Present -> Presence.Present value.Value
        | state -> invalidArg (nameof value) $"Unknown presence state {state}."

    let toCore (value: Presence<'T>) : Valksor.Naatre.Presence<'T> =
        match value with
        | Presence.Missing -> Valksor.Naatre.Presence<'T>.Missing
        | Presence.Null -> Valksor.Naatre.Presence<'T>.Null
        | Presence.Failed errors -> Valksor.Naatre.Presence<'T>.Failed errors
        | Presence.Skipped reason -> Valksor.Naatre.Presence<'T>.Skipped reason
        | Presence.Pending -> Valksor.Naatre.Presence<'T>.Pending
        | Presence.Present item -> Valksor.Naatre.Presence<'T>.Present item

    let ofOption (value: 'T option) : Presence<'T> =
        match value with
        | None -> Presence.Missing
        | Some item when isNull (box item) -> Presence.Null
        | Some item -> Presence.Present item

[<RequireQualifiedAccess>]
module Operation =
    let canonicalRequest (operation: NaatreOperation<'T>) = operation.CanonicalRequest

    let executeAsync
        (client: INaatreClient)
        (cancellationToken: CancellationToken)
        (operation: NaatreOperation<'T>) =
        client.ExecuteAsync(operation, cancellationToken) |> Async.AwaitTask

    let stream
        (client: INaatreClient)
        (cancellationToken: CancellationToken)
        (operation: NaatreOperation<'T>) =
        client.StreamAsync(operation, cancellationToken)
