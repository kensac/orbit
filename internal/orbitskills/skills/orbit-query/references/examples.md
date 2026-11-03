# Query examples

Spend per category in October, transfers left out:

```json
{"file": "inputs/tx.json", "items": "transactions",
 "where": [{"field": "date", "op": "gte", "value": "2026-10-01"}, {"field": "date", "op": "lt", "value": "2026-11-01"},
           {"field": "category", "op": "ne", "value": "Transfer"}],
 "group_by": ["category"], "aggregates": [{"op": "sum", "field": "amount"}, {"op": "count"}]}
```

The five largest charges, to quote them:

```json
{"file": "inputs/tx.json", "items": "transactions", "where": [{"field": "amount", "op": "gt", "value": 500}],
 "select": ["date", "merchant", "amount"], "limit": 5}
```

How many records have a field at all:

```json
{"file": "inputs/tx.json", "items": "transactions", "where": [{"field": "receipt", "op": "exists"}]}
```
