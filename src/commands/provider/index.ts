import type { Command } from '../../commands.js'

const provider = {
  aliases: ['apikey'],
  type: 'local-jsx',
  name: 'provider',
  description:
    'Configure the model provider (endpoint, API key, models) in ~/.ultrac/settings.json',
  load: () => import('./provider.js'),
} satisfies Command

export default provider
