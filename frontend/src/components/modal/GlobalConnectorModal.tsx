"use client"

import { ConnectorConfigModal } from "./ConnectorConfigModal"
import { useConnectorConfigModal } from "@/lib/hooks/useConnectorConfigModal"

export function GlobalConnectorModal() {
  const modal = useConnectorConfigModal()
  
  return (
    <ConnectorConfigModal
      open={modal.isOpen}
      onClose={modal.closeModal}
      connectorType={modal.connectorType || ""}
      direction={modal.direction || "source"}
      onSave={(connectionId, config) => {
        modal.onSaveCallback?.(connectionId, config)
        modal.closeModal()
      }}
    />
  )
}

