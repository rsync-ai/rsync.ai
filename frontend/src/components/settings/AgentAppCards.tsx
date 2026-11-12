"use client"

import type { ReactNode } from "react"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { agentAppSnippets, agentGatewayUrl } from "@/lib/api/agent-tokens"
import { CopyBlock } from "./CopyBlock"

/**
 * Pick your AI app, see its steps: free apps first
 * (docs/internal/rsync-assistant-design.md § 4.5). Every app listed works today
 * with a token; the apps that will connect by signing in are named on one line
 * below. No step ever holds a token: each setup uses a placeholder the app
 * fills in, and the token itself is shown once, when it is created.
 */

type App = {
  id: string
  name: string
  plan: string
  steps: ReactNode[]
  snippet?: (url: string) => { label: string; value: string }
}

const code = (text: string) => <code className="font-mono text-[0.85em]">{text}</code>

const APPS: App[] = [
  {
    id: "le-chat",
    name: "Mistral Le Chat",
    plan: "Free plan",
    steps: [
      "Create a token under Agent tokens. Le Chat keeps it, so give it a short expiry.",
      "In Le Chat, open Connectors → Add connector → Custom MCP and paste the address below.",
      "Choose API key / Bearer authentication and paste the token.",
    ],
    snippet: (url) => ({ label: "MCP address", value: url }),
  },
  {
    id: "vscode",
    name: "Copilot in VS Code",
    plan: "Free plan, with a monthly limit",
    steps: [
      <>Save the file below as {code(".vscode/mcp.json")}.</>,
      "Create a token under Agent tokens. VS Code asks for it when it starts the server, and does not save it in the file.",
    ],
    snippet: (url) => ({ label: ".vscode/mcp.json", value: agentAppSnippets(url).vscode }),
  },
  {
    id: "gemini-cli",
    name: "Gemini CLI",
    plan: "Free plan",
    steps: [
      <>Add the block below to {code("~/.gemini/settings.json")}.</>,
      <>
        Create a token under Agent tokens and set {code("RSYNC_AGENT_TOKEN")} to it in your shell. The file names the
        variable, not the token.
      </>,
    ],
    snippet: (url) => ({ label: "~/.gemini/settings.json", value: agentAppSnippets(url).geminiCli }),
  },
  {
    id: "claude-code",
    name: "Claude Code",
    plan: "Paid plans",
    steps: [
      "Create a token under Agent tokens.",
      <>Paste the {code("claude mcp add")} command shown with it into a terminal.</>,
    ],
  },
  {
    id: "cursor",
    name: "Cursor",
    plan: "Paid plans",
    steps: [
      "Create a token under Agent tokens.",
      <>Save the file shown with it as {code(".cursor/mcp.json")}.</>,
    ],
  },
]

const SIGN_IN_APPS = "Claude (claude.ai), the Gemini app and ChatGPT"

export function AgentAppCards() {
  const url = agentGatewayUrl()
  return (
    <div className="space-y-4">
      <Tabs defaultValue={APPS[0].id}>
        <TabsList aria-label="AI app" className="h-auto w-full flex-wrap justify-start gap-1">
          {APPS.map((app) => (
            <TabsTrigger key={app.id} value={app.id}>
              {app.name}
            </TabsTrigger>
          ))}
        </TabsList>
        {APPS.map((app) => {
          const snippet = app.snippet?.(url)
          return (
            <TabsContent key={app.id} value={app.id} className="mt-4 space-y-3 text-sm">
              <p className="text-xs text-zinc-500 dark:text-zinc-400">{app.plan}</p>
              <ol className="list-decimal space-y-1.5 pl-5 text-zinc-700 marker:text-zinc-500 dark:text-zinc-300">
                {app.steps.map((step, i) => (
                  <li key={i}>{step}</li>
                ))}
              </ol>
              {snippet && <CopyBlock label={snippet.label} value={snippet.value} />}
            </TabsContent>
          )
        })}
      </Tabs>
      <p className="text-xs text-zinc-500 dark:text-zinc-400">
        Coming: connect from {SIGN_IN_APPS} by signing in to rsync.ai, with no token to copy.
      </p>
    </div>
  )
}
