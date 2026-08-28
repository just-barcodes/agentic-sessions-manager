// Forwards opencode session lifecycle events to the `sm` daemon by piping
// each event's JSON to `sm hook opencode` on stdin.
//
// Install: cp this file to ~/.config/opencode/plugin/sm.ts
// Override the binary path with SM_BIN if `sm` is not on PATH.
import type { Plugin } from "@opencode-ai/plugin"

const SM_BIN = process.env.SM_BIN ?? "sm"

const FORWARD = new Set(["permission.asked", "session.idle", "session.error"])

const send = async (payload: unknown) => {
  try {
    const proc = Bun.spawn([SM_BIN, "hook", "opencode"], {
      stdin: "pipe",
      stdout: "ignore",
      stderr: "ignore",
    })
    proc.stdin.write(JSON.stringify(payload))
    proc.stdin.end()
    await proc.exited
  } catch {
    // never break the user's session
  }
}

export const SmTracker: Plugin = async () => {
  const announced = new Map<string, string>()
  return {
    event: async ({ event }) => {
      const props = (event as any)?.properties
      const sid: unknown = props?.sessionID
      if (typeof sid !== "string" || !sid) return

      if (event.type === "session.created" || event.type === "session.updated") {
        // opencode fires session.updated repeatedly, so forward only the first
        // (which announces the session) and any later one whose title changed —
        // it names a session after its first message, well after the start.
        // Those go out as session.title so sm records the name without
        // replaying a session start, which would reset a live turn to idle.
        const title: string = typeof props?.info?.title === "string" ? props.info.title : ""
        const seen = announced.has(sid)
        if (seen && announced.get(sid) === title) return
        announced.set(sid, title)
        await send({ type: seen ? "session.title" : "session.updated", properties: props })
        return
      }
      if (FORWARD.has(event.type)) {
        await send(event)
      }
    },
  }
}

export default SmTracker
