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
  const announced = new Set<string>()
  return {
    event: async ({ event }) => {
      const props = (event as any)?.properties
      const sid: unknown = props?.sessionID
      if (typeof sid !== "string" || !sid) return

      if (event.type === "session.created" || event.type === "session.updated") {
        if (announced.has(sid)) return
        announced.add(sid)
        await send({ type: "session.updated", properties: props })
        return
      }
      if (FORWARD.has(event.type)) {
        await send(event)
      }
    },
  }
}

export default SmTracker
