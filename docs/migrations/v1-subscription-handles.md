# v1 secure subscription-handle migration note

Native EventSource integrations must no longer encode operation documents,
variables, bearer credentials, or replay cursors in a delivery URL. Establish
the subscription with authenticated POST, retain the returned snapshot and
cursor, and attach through either Fetch bearer delivery or the same-origin
cookie EventSource path advertised by the response.

Servers must treat existing opaque stream URLs as identifiers only, migrate
their backing records to the complete principal, tenant, operation, variable,
schema, authorization, limit, time, and delivery-profile binding, and
reauthorize every action and protected frame. Revision drift requires
re-establishment. Cookie-authenticated state changes require exact-origin CSRF
defense; all responses are no-store and non-redirecting.

Broker integrations must publish fidelity rather than assume it. Missing
history-loss detection, terminal retention, replay authorization, duplicate
classification, private delivery, or bounded retry is an explicit unsupported
mapping. Mercure is optional and is not a Naatre wire-compatibility claim.
