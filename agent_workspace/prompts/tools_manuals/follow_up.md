## Tool: Follow Up (`follow_up`)

Schedule an independent background prompt to be executed by yourself immediately after responding to the user. Use this for sequential, multi-step work without forcing the user to wait or re-prompt you.

### Constraints

- Maximum 10 sequential follow-ups before the system forces a pause.
- You CAN include a natural-language response to the user AND append this tool call at the end. The user sees your text; the follow-up triggers immediately afterward.

### Schema

| Parameter | Type | Required | Description |
|---|---|---|---|
| `task_prompt` | string | yes | Description of the next step to execute |

### Example

```json
{"action": "follow_up", "task_prompt": "Continue with Phase 3 of the refactoring plan now."}
```