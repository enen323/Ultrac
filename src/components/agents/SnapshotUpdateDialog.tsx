// TODO: Phase 6 - 还原 SnapshotUpdateDialog

import React from 'react'

export function SnapshotUpdateDialog(props: {
  agentType: string
  scope: unknown
  snapshotTimestamp: string
  onComplete: (result: 'merge' | 'keep' | 'replace') => void
  onCancel: () => void
}): React.ReactElement {
  return React.createElement('div', null, 'SnapshotUpdateDialog stub')
}
