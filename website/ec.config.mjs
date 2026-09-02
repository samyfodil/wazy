import { defineEcConfig } from '@astrojs/starlight/expressive-code';
import { pluginCollapsibleSections } from '@expressive-code/plugin-collapsible-sections';

// Expressive Code options must live here rather than in astro.config.mjs: the
// landing page uses the <Code> component, and that path requires the options to
// be JSON-serialisable, which a plugin instance is not.
export default defineEcConfig({
  themes: ['github-dark-dimmed', 'github-light'],
  // Lets a tutorial show a whole real file while folding the imports and
  // boilerplate a reader does not need to re-read.
  plugins: [pluginCollapsibleSections()],
});
