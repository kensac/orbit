# Entry examples

Every field is present in each write; one a write does not use is an empty string.

A correction, kept as feedback for every agent:

```json
{"op": "add", "ref": "profile", "type": "feedback", "title": "Transfers are not spending",
 "description": "Leave transfers between own accounts out of spending totals.",
 "body": "Transfers between the person's own accounts are not spending.", "why": "They corrected an October total.",
 "how_to_apply": "Filter category Transfer out before summing.", "superseded_by": "", "note": "They said a transfer was counted as spending."}
```

A fact that changed, the new entry added in the same result:

```json
{"op": "add", "ref": "profile", "type": "user", "title": "Lives in Queens", "description": "Home is Queens, NYC.",
 "body": "The person lives in Queens.", "why": "", "how_to_apply": "", "superseded_by": "", "note": "They said they moved."}
{"op": "retire", "ref": "profile/lives-in-boston", "type": "", "title": "", "description": "", "body": "", "why": "",
 "how_to_apply": "", "superseded_by": "profile/lives-in-queens", "note": "They moved."}
```
