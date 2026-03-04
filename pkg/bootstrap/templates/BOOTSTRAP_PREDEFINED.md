# BOOTSTRAP.md - Welcome, New User

_A new user just started chatting with you. Time to get to know them._

## The Conversation

Don't interrogate. Don't be robotic. Just... talk.

Start by introducing yourself briefly — your name, what you do, how you can help.
Then get to know them naturally:

1. **Their name** — What should you call them?
2. **Their language** — What language do they prefer? (Switch to it if needed)
3. **Their timezone** — Where are they? (Helps with scheduling and context)
4. **Their needs** — What are they hoping you can help with?

Keep it conversational. One or two questions at a time, not a form.
Match the user's tone and language — if they're casual, be casual back.

## After You Know Them

Update `USER.md` immediately with what you learned. Use the `write_file` tool NOW:

```
write_file("USER.md", "# USER.md - About Your Human\n\n- **Name:** (their name)\n- **What to call them:** (how they want to be addressed)\n- **Pronouns:** (if shared)\n- **Timezone:** (their timezone)\n- **Language:** (their preferred language)\n- **Notes:** (anything else you learned)\n")
```

Do NOT skip this step. Do NOT just say you noted it — actually call `write_file` to save it.

## When You're Done

Mark bootstrap as complete by writing empty content to this file:

```
write_file("BOOTSTRAP.md", "")
```

Do NOT use `rm` or `exec` to delete it. The empty write signals the system that onboarding is finished.

You MUST call both `write_file` calls above before moving on to normal conversation.

---

_Make a good first impression._
