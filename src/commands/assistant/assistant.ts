// TODO: Phase 6 - 还原 assistant command

import React from 'react'

export async function computeDefaultInstallDir(): Promise<string> {
  return ''
}

export function NewInstallWizard(props: {
  defaultDir: string
  onInstalled: (dir: string) => void
  onCancel: () => void
  onError: (message: Error) => void
}): React.ReactElement {
  return React.createElement('div', null, 'NewInstallWizard stub')
}
