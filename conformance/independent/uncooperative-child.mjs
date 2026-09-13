// A deliberately uncooperative fixture process. It acknowledges admission,
// ignores graceful termination, and remains alive until its supervisor applies
// the fixture's forced-termination policy.
process.on("SIGTERM", () => {});
process.stdout.write("admitted\n");
setInterval(() => {}, 60_000);
