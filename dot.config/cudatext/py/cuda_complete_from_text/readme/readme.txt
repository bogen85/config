Plugin for CudaText.
Handles auto-completion command (default hotkey: Ctrl+Space).
Gives completion listbox with words from current document, starting with the current word (under caret).
E.g. if you typed "op", it may show words "operations", "opinion", "option" etc.
Plugin cannot work with multi-carets. 

Plugin has options in the config file (settings/plugins.ini). To open the config file, call menu item "Options / Settings-pligins / Complete From Text". Options are: 

- 'lexers': comma-separated lexer names, ie for which lexers to work; specify none-lexer as '-'.
- 'min_len': minimal word length, words of smaller length will be ignored.
- 'max_lines': if document has bigger count of lines, ignore this document.    
- 'case_sens': case-sensitive; words starting with 'A' will be ignored when you typed 'a'.
- 'what_editors': which documents (ie UI tabs) to read to get words. Values:
    0: only current document.
    1: all opened documents.
    2: all opened documents with the same lexer.
- 'use_acp': add suggestions from autocomplete files 'data/autocomplete/*.acp'
- 'show_acp_first': when option is On, items from 'data/autocomplete/*.acp' files will be listed first in the completion listbox
- 'case_split': expands suggestions, autocompletion for 'AB' will include 'AzzBzz', so to get 'ValueError' just typing 'VE' (or 'VaE', 'VErr', 'ValErr' etc.) and calling on autocompletion will suggest it.
- 'underscore_split': expands suggestions, autocompletion for 'AB' will include 'AZZ_BZZ', so to get 'supports_bytes_environ' just typing 'sb' (or 'sbe', 'supbyenv' etc.) and calling on autocompletion will suggest it. 
- 'fuzzy_search': activates fuzzy matching for words; if it's on it has bigger priority than 'case_split' and 'underscore_split'.


Plugin supports CudaText option "nonword_chars", so for example words with '$' can be added,
if option is configured so.


Authors:
  Alexey Torgashin (CudaText)
  Shovel (CudaText forum user), https://github.com/halfbrained
License: MIT
