import * as React from 'react'
import { ConfigurableShortcutHint } from '../../components/ConfigurableShortcutHint.js'
import { Byline } from '../../components/design-system/Byline.js'
import { KeyboardShortcutHint } from '../../components/design-system/KeyboardShortcutHint.js'
import TextInput from '../../components/TextInput.js'
import { WizardProvider } from '../../components/wizard/index.js'
import { WizardDialogLayout } from '../../components/wizard/WizardDialogLayout.js'
import { useWizard } from '../../components/wizard/useWizard.js'
import { Box, Text, useInput } from '../../ink.js'
import { useKeybinding } from '../../keybindings/useKeybinding.js'
import type {
  LocalJSXCommandCall,
  LocalJSXCommandOnDone,
} from '../../types/command.js'
import { clearApiKeyHelperCache } from '../../utils/auth.js'
import { isEnvTruthy } from '../../utils/envUtils.js'
import { applyConfigEnvironmentVariables } from '../../utils/managedEnv.js'
import {
  getSettings_DEPRECATED,
  getSettingsFilePathForSource,
  updateSettingsForSource,
} from '../../utils/settings/settings.js'
import type { SettingsJson } from '../../utils/settings/types.js'

/**
 * Values collected by the wizard, keyed by field key (not by env var name).
 * Written into the `env` block of the user-scoped settings file.
 */
type ProviderData = Record<string, unknown>

type FieldSpec = {
  /** Key inside ProviderData. */
  key: string
  /** Environment variable this field maps to. */
  envVar: string
  label: string
  hint: string
  placeholder: string
  /** When set, the input renders masked (password-style). */
  mask?: string
}

const FIELDS: FieldSpec[] = [
  {
    key: 'baseUrl',
    envVar: 'ANTHROPIC_BASE_URL',
    label: 'Base URL',
    hint: 'Anthropic-compatible API endpoint. Leave blank to fall back to api.anthropic.com.',
    placeholder: 'https://api.deepseek.com/anthropic',
  },
  {
    key: 'authToken',
    envVar: 'ANTHROPIC_AUTH_TOKEN',
    label: 'API key',
    hint: 'Sent as the Authorization: Bearer header. Stored as plain text in the settings file.',
    placeholder: 'sk-...',
    mask: '*',
  },
  {
    key: 'model',
    envVar: 'ANTHROPIC_MODEL',
    label: 'Main model',
    hint: 'Model id used by the main agent loop.',
    placeholder: 'deepseek-flash',
  },
  {
    key: 'smallFastModel',
    envVar: 'ANTHROPIC_SMALL_FAST_MODEL',
    label: 'Fast model',
    hint: 'Small / fast model used for background work.',
    placeholder: 'deepseek-flash',
  },
]

const SETTINGS_SOURCE = 'userSettings' as const

/** Mask a secret for display: keep a short head and tail, hide the middle. */
function maskSecret(value: string): string {
  if (value.length <= 12) return '*'.repeat(value.length)
  return `${value.slice(0, 6)}${'*'.repeat(Math.min(value.length - 10, 16))}${value.slice(-4)}`
}

/**
 * Read the value we should prefill: the settings file wins over process.env,
 * because the settings file is what this wizard owns.
 */
function readStoredEnv(envVar: string): string {
  try {
    const stored = (
      getSettings_DEPRECATED() as { env?: Record<string, string> } | undefined
    )?.env?.[envVar]
    if (stored !== undefined && stored !== null) return String(stored)
  } catch {
    // Settings could not be read — fall through to process.env.
  }
  return process.env[envVar] ?? ''
}

function readInitialData(): ProviderData {
  const data: ProviderData = {}
  for (const field of FIELDS) {
    data[field.key] = readStoredEnv(field.envVar)
  }
  return data
}

/**
 * Persist the wizard result and make it take effect in the current session.
 *
 * An empty field is written as an explicit `undefined`, which
 * `updateSettingsForSource` treats as "delete this key" — so clearing a field
 * removes it from the file instead of leaving an empty string behind.
 */
function saveProviderSettings(data: ProviderData): {
  error: Error | null
  filePath: string | null
} {
  const filePath = getSettingsFilePathForSource(SETTINGS_SOURCE)
  const env: Record<string, string | undefined> = {}
  for (const field of FIELDS) {
    const raw = String(data[field.key] ?? '').trim()
    env[field.envVar] = raw === '' ? undefined : raw
  }

  const { error } = updateSettingsForSource(SETTINGS_SOURCE, {
    env,
  } as unknown as SettingsJson)
  if (error) return { error, filePath }

  try {
    // Re-apply env so the change lands in the running session, then drop the
    // api-key caches so the next API call rebuilds its auth headers.
    applyConfigEnvironmentVariables()
    clearApiKeyHelperCache()
  } catch (e) {
    return { error: e instanceof Error ? e : new Error(String(e)), filePath }
  }

  return { error: null, filePath }
}

/** Footer shown under every field step. */
function fieldFooter(): React.ReactNode {
  return (
    <Byline>
      <KeyboardShortcutHint shortcut="Enter" action="continue" />
      <ConfigurableShortcutHint
        action="confirm:no"
        context="Settings"
        fallback="Esc"
        description="go back"
      />
    </Byline>
  )
}

/**
 * Build one text-field step. A distinct component identity per field keeps
 * React from reusing local state when the wizard moves between steps.
 */
function makeFieldStep(spec: FieldSpec): React.ComponentType {
  function FieldStep(): React.ReactNode {
    const { wizardData, updateWizardData, goNext, goBack } =
      useWizard<ProviderData>()
    const initial = String(wizardData[spec.key] ?? '')
    const [value, setValue] = React.useState(initial)
    const [cursorOffset, setCursorOffset] = React.useState(initial.length)

    useKeybinding('confirm:no', goBack, { context: 'Settings' })

    const handleSubmit = (submitted: string) => {
      updateWizardData({ [spec.key]: submitted.trim() })
      goNext()
    }

    return (
      <WizardDialogLayout subtitle={spec.label} footerText={fieldFooter()}>
        <Box flexDirection="column">
          <Text>{spec.hint}</Text>
          <Box marginTop={1} flexDirection="column">
            <Text dimColor>{spec.envVar}</Text>
            <TextInput
              value={value}
              onChange={setValue}
              onSubmit={handleSubmit}
              placeholder={spec.placeholder}
              mask={spec.mask}
              columns={80}
              cursorOffset={cursorOffset}
              onChangeCursorOffset={setCursorOffset}
              focus={true}
              showCursor={true}
            />
          </Box>
        </Box>
      </WizardDialogLayout>
    )
  }
  return FieldStep
}

const FIELD_STEPS = FIELDS.map(makeFieldStep)

/** Last step: show what will be written, save on Enter. */
function ConfirmStep(): React.ReactNode {
  const { wizardData, goNext, goBack } = useWizard<ProviderData>()

  useKeybinding('confirm:no', goBack, { context: 'Settings' })
  useInput((_input, key) => {
    if (key.return) goNext()
  })

  const filePath = getSettingsFilePathForSource(SETTINGS_SOURCE)
  const warnings: string[] = []

  if (isEnvTruthy(process.env.CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST)) {
    warnings.push(
      'CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST is set — it strips provider settings from every settings source, so these values will NOT take effect until it is unset.',
    )
  }
  if (!String(wizardData.authToken ?? '').trim()) {
    warnings.push(
      'No API key configured — requests will fail unless another auth source (e.g. apiKeyHelper) is set.',
    )
  }

  const rows = FIELDS.map(field => {
    const raw = String(wizardData[field.key] ?? '').trim()
    const display =
      raw === ''
        ? '(cleared)'
        : field.mask
          ? maskSecret(raw)
          : raw
    return { envVar: field.envVar, display }
  })

  return (
    <WizardDialogLayout
      subtitle="Save and apply"
      footerText={
        <Byline>
          <KeyboardShortcutHint shortcut="Enter" action="save" />
          <ConfigurableShortcutHint
            action="confirm:no"
            context="Settings"
            fallback="Esc"
            description="go back"
          />
        </Byline>
      }
    >
      <Box flexDirection="column">
        <Text>
          Writes to <Text bold>{filePath ?? '(settings file unavailable)'}</Text>
        </Text>
        <Box marginTop={1} flexDirection="column">
          {rows.map(row => (
            <Text key={row.envVar}>
              <Text dimColor>{row.envVar}</Text> = {row.display}
            </Text>
          ))}
        </Box>
        {warnings.length > 0 && (
          <Box marginTop={1} flexDirection="column">
            {warnings.map(warning => (
              <Text key={warning} color="warning">
                ! {warning}
              </Text>
            ))}
          </Box>
        )}
      </Box>
    </WizardDialogLayout>
  )
}

const STEPS: React.ComponentType[] = [...FIELD_STEPS, ConfirmStep]

function ProviderWizard({
  onDone,
}: {
  onDone: LocalJSXCommandOnDone
}): React.ReactNode {
  return (
    <WizardProvider
      steps={STEPS}
      initialData={readInitialData()}
      onComplete={data => {
        const { error, filePath } = saveProviderSettings(data as ProviderData)
        if (error) {
          onDone(`Failed to save provider settings: ${error.message}`)
          return
        }
        onDone(
          `Model provider saved to ${filePath}. Applied to the current session.`,
        )
      }}
      onCancel={() => onDone('Provider configuration cancelled.')}
      title="Model provider"
    />
  )
}

export const call: LocalJSXCommandCall = async (onDone): Promise<React.ReactNode> => {
  return <ProviderWizard onDone={onDone} />
}
