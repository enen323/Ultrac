// TODO: Phase 6 - 还原 AssistantSessionChooser

import React from 'react'

export function AssistantSessionChooser(props: {
  sessions: unknown[]
  onSelect: (id: string) => void
  onCancel: () => void
}): React.ReactElement {
  return React.createElement('div', null, 'AssistantSessionChooser stub')
}
