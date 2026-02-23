# Multilingual Prompts Reference

This directory contains prompt templates in various languages for future implementation of the multilingual prompt management system.

## Status

These prompts are currently **reference only** and not yet integrated into the application. See [Issue #2](https://github.com/Cyb3rDudu/axiom-private/issues/2) for the implementation plan.

## Current Prompts

### German (de/)
- `research_agent_de.txt` - Research Agent system prompt
- `planning_agent_de.txt` - Planning Agent phase 1 system prompt  
- `writing_agent_de.txt` - Writing Agent system prompt

**Tested:** ✅ Successfully validated in production with Mission `ef1cab00-b201-4b45-80ab-cc8e85a31680`
- Research quality: Excellent
- Source citation: Maintained
- Output: Native German, academically sound

## Future Languages

- [ ] French (fr/)
- [ ] Spanish (es/)
- [ ] Portuguese (pt/)
- [ ] Italian (it/)
- [ ] Dutch (nl/)

## Usage

Once the multilingual system is implemented (Issue #2), these prompts will be:
1. Migrated to the `prompt_templates` database table
2. Associated with language code 'de'
3. Available for selection during mission creation

## Contributing

To add a new language:
1. Create a directory with the ISO 639-1 language code (e.g., `fr/` for French)
2. Translate the three core agent prompts
3. Test thoroughly with a representative mission
4. Submit a pull request with test results

## Notes

- Keep prompt structure intact (placeholders, special instructions)
- Maintain the same level of detail as English prompts
- Test with domain-specific documents in the target language
- Ensure LaTeX/math syntax works correctly
