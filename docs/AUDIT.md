# Audit durability policy

Which successful operations are allowed to exist without a durable audit
record, and what happens when the record cannot be written.

The question is not academic. Before this policy every audit write was
best-effort: the record was a second statement on a second connection, and a
failure was a log line. That is right for most of the platform and wrong for
the part of it that decides who may see what.

## The test

One question decides the category:

> If this record is missing, can anyone afterwards establish that the action
> happened, and who asked for it?

For most of the platform the answer is yes — the notification is still there,
the incident is still there, the Global ID is still there. The audit row is a
convenience for reading a trail, not the only evidence.

For an authorization change the answer is **no**. The platform keeps no other
trace of who granted a scope to whom: `principal_scopes` holds the grant, not
its provenance. A grant that took effect without a record cannot be
reconstructed from anything, which makes it indistinguishable from a grant
nobody made.

## The categories

| Action | Category | Policy |
| --- | --- | --- |
| `rbac.scope.granted` | security-critical mutation | **fail-closed**, atomic |
| `rbac.scope.revoked` | security-critical mutation | **fail-closed**, atomic |
| `global_id.created` | business mutation | fail-open, counted |
| `incident.created` | business mutation | fail-open, counted |
| `incident.updated` | business mutation | fail-open, counted |
| `global_id.read` | sensitive read | fail-open, counted |
| `ai.request` | sensitive read | fail-open, counted |
| `notification.read` | informational | fail-open, counted |

Nothing here is an owner decision waiting to be made. Each row follows from an
invariant the platform already has: authorization is deny-by-default and
backend-enforced, so the record of a change to it is part of the enforcement.
Everything else describes state the platform stores anyway.

Whether a *retention period* applies to these records, and how long, is a
business decision and is not made here.

## Fail-closed

`AddScopeGrant` and `DeleteScopeGrant` write the grant and its audit record in
**one transaction**. Either both rows exist or neither does. When the audit
write fails, the change is rolled back and the caller gets:

```json
{
  "error": "the grant was refused because its audit record could not be written",
  "code": "audit_unavailable",
  "request_id": "..."
}
```

`503`, not `500`: the request is worth retrying once the audit store is
healthy. The message says the change did not happen, because an administrator
who believes a grant succeeded will not make it again. It carries no database
detail — this endpoint decides who may see what, and a raw error here names
tables and constraints.

This is only possible because both rows live in the same database. Atomicity
is the right answer where it is available; it is not available everywhere, and
pretending otherwise would mean a distributed transaction across systems that
do not have one.

## Fail-open

Every other audited action succeeds whether or not its record was written.
That is a deliberate trade and not an oversight: a platform that refuses every
request while its audit table is unwell has turned a bookkeeping failure into
an outage, and the actions in that list all leave their own evidence behind.

The failure is **counted, not just logged**:

```
bsystem_audit_writes_total{action="global_id.read",outcome="failed"}
```

That metric exists because the alternative is invisible. An audit trail that
has silently stopped being written looks exactly like a quiet week on every
dashboard that counts only what happened.

### Suggested alert

```promql
increase(bsystem_audit_writes_total{outcome=~"failed|refused"}[15m]) > 0
```

`failed` is a record that was not written on a fail-open path — the action
happened and is not attributable. `refused` is a fail-closed path that turned
an audit failure into a refusal — the action did not happen. Both need a
person; they need different people.

## What is executed

- `TestAScopeGrantIsRefusedWhenItsAuditRecordCannotBeWritten` — the audit table
  is made to refuse writes, and the grant is refused, says so without
  describing the database, and **does not appear in the principal's scopes**.
  A `503` that left the scope granted would be worse than a `201`, because
  nobody would go looking for it.
- `TestALowRiskPathStillWorksWhenTheAuditSinkIsBroken` — the same broken audit
  table, and an audited read plus an ordinary request both still answer `200`,
  with the failure counted.

Both were verified by mutation: writing the audit record outside the grant's
transaction makes the first fail, and swallowing the insert error makes the
second fail.
