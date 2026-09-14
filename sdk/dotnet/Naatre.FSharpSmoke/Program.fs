open Valksor.Naatre
open Valksor.Naatre.Generated

[<EntryPoint>]
let main _ =
    let variables = GetAccountVariables(Id = "acct-1", Nickname = Presence<string>.Null)
    let operation = GetAccount.Create variables
    let unknown = Status.Parse "FUTURE_STATUS"

    if operation.Name <> "GetAccount" || unknown.Value.IsKnown || unknown.Value.Raw <> "FUTURE_STATUS" then
        failwith "C# generated shapes are not usable from F#"

    0
