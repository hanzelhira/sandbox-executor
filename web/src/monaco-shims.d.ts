// Monaco's basic-language contributions are side-effect-only ES modules and
// ship no type declarations. They register a tokenizer with the editor and
// export nothing, so an ambient declaration is all TypeScript needs.
declare module 'monaco-editor/languages/definitions/*'
