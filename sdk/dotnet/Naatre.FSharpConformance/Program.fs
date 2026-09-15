open System
open System.Globalization
open System.Text.Json
open Valksor.Naatre
open Valksor.Naatre.Generated
open Valksor.Naatre.FSharp

[<EntryPoint>]
let main _ =
    let missing = Presence.ofCore Valksor.Naatre.Presence<string>.Missing
    let nullValue = Presence.ofCore Valksor.Naatre.Presence<string>.Null
    let present = Presence.ofCore (Valksor.Naatre.Presence<string>.Present "Ada")

    match missing, nullValue, present with
    | Presence.Missing, Presence.Null, Presence.Present "Ada" -> ()
    | _ -> failwith "missing, null, and present states collapsed"

    match Presence.ofOption (None: string option), Presence.ofOption (Some null), Presence.ofOption (Some "Ada") with
    | Presence.Missing, Presence.Null, Presence.Present "Ada" -> ()
    | _ -> failwith "F# option, null, and value states collapsed"

    let roundTrip = present |> Presence.toCore |> Presence.ofCore
    if roundTrip <> present then
        failwith "presence conversion did not round-trip"

    let variables = GetAccountVariables(Id = "acct-1", Nickname = Valksor.Naatre.Presence<string>.Null)
    let operation = GetAccount.Create variables
    let sharedBytes = Operation.canonicalRequest operation
    if operation.CanonicalRequest.ToArray() <> sharedBytes.ToArray() then
        failwith "F# did not use the canonical C# codec"

    let previousCulture = CultureInfo.CurrentCulture
    let previousUICulture = CultureInfo.CurrentUICulture
    try
        CultureInfo.CurrentCulture <- CultureInfo.GetCultureInfo "tr-TR"
        CultureInfo.CurrentUICulture <- CultureInfo.GetCultureInfo "fr-FR"
        let cultureOperation = GetAccount.Create variables
        if cultureOperation.CanonicalRequest.ToArray() <> sharedBytes.ToArray() then
            failwith "culture changed F# canonical request bytes"
    finally
        CultureInfo.CurrentCulture <- previousCulture
        CultureInfo.CurrentUICulture <- previousUICulture

    printfn "%s" (JsonSerializer.Serialize {| profile = "sdk.dotnet.adapters-1"; status = "passed"; surface = "fsharp" |})
    0
