import { AgenticChatInterfaceV2 } from "@/components/chat/AgenticChatInterfaceV2"

export const dynamic = "force-dynamic"

export default function ChatPage() {
  return (
    // Fills the workspace frame DashboardShell sizes to the window; the chat's
    // own panes (messages, input, DAG panel) split that height between them.
    <div className="flex min-h-0 flex-1 flex-col">
      {/* AgenticChatInterfaceV2 already renders a dedicated home/hero state. */}
      <AgenticChatInterfaceV2 />
    </div>
  )
}

