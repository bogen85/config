Plugin for CudaText.
"Terminal Plus" gives Terminal emulation, it's continuation of plugin "Terminal".
On Linux (maybe other Unixes too) it supports colored output in terminal.
On Windows colors are not supported, because it's not needed much
(only few PowerShell commands give colored output).

Plugin gives its panel in the CudaText bottom panel.
Plugin shows its own statusbar (inside its panel) with cells for all opened terminals.
Additionally plugin shows buttons on CudaText sidebar, one button per terminal.
Ie it shows 2 equal rows of buttons, but it may be useful if CudaText sidebar is hidden.


Mapping of terminals
--------------------
Plugin maps its terminals to documents: it remembers which terminal is opened
for which document. "Toggle focus" command does this:

- from named document, if it has terminal(s) attached - puts focus to attached terminal;
- from untitled document - does nothing;
- from terminal attached to some document - puts focus to document;
- from 'free' terminal (in OS home dir) - does nothing.

CudaText command "Save as" is not handled, new file name will be without terminals.


Plugin commands
---------------
- "Open": activates Terminal panel (it initially doesn't have terminals)

- "Add terminal for user home directory": adds 'free' terminal, not attached to any document
  (opens in user's home directory)
- "Add terminal (for current file)": adds terminal attached to current named document
  (one document may have multiple terminals)

- "Rename terminal": allows to set caption of terminal for plugin's statusbar

- "Close terminal": closes currently focused terminal (it may be 'free' terminal)
- "Close last terminal for current file"
- "Close all terminals"

- "Focus next terminal"
- "Focus previous terminal"

- "Toggle focus: editor - its terminal": switches focus between current document
  and its last attached terminal.
  If current document doesn't have terminals, do nothing.
  If current terminal is 'free', do nithing.

- "Execute selected text in terminal".
  Ignores multi-line selection.
  Ignores multi-carets.
  If no selection, runs current line and moves editor caret to the next line.

- "Search for command in history": in the plugin's input field (below the
  terminal output), shows menu of all recent commands,
  filtered by currently typed text.


Plugin features
---------------

Plugin loads ~/.bashrc if Bash is used, so Bash aliases must work.

Output of interactive programs like 'less' or 'git diff':
- listing can be scrolled, but only down, with Down and PageDown keys
- to stop interactive program, enter command "q" (with Enter)

Plugin shows special cells in the statusbar:
- "+": runs command "Add terminal for current file"
- "X": runs command "Close all terminals"

In the plugin's statusbar, cells are sorted:
- first: 'free' terminals without a file
- with editor tabs opened (these are sorted by order of their UI tabs)
- last: terminals for files without an UI tab (closed files)

Colored lines on the terminal cells show:
- bright line on top - selected terminal
- bright line at the bottom - terminal for currently selected document
- dark line at the bottom - to separate terminals for different documents
- red line at the bottom - terminal's document is current not opened in CudaText

Context menu for plugin's statusbar cells allows:
- to rename terminal
- to change its icon
- to change its wrap mode
- to close it

Custom icons (resolution of 20x20) may be placed into CudaText directory
"data/terminalicons". This folder is searched additionally to plugin's folder.

Terminals are restored after app restart, the state is saved to file
"cuda_terminal_plus_state.json" in the CudaText "settings" folder.


Plugin options
--------------

- 'shell_windows', 'shell_unix', 'shell_macos': Shell start command
  for designated OS types. May include arguments.

- 'start_dir': Working directory of new terminals. Possible values:
    - 'file': in document's directory
    - 'project': in current project's main-file's directory
    - 'user': in user's home directory

- 'floating_window': Show plugin in a separate window.
- 'floating_window_topmost': Should floating window be 'always on top'.
- 'layout_horizontal': Places terminals bar to the right of the input.
   Values are 0 to disable, 1 to enable.
   
- 'font_size': Terminal font size

- 'shell_theme_fg': Terminal font colors.
  Comma-separated list of HTML colors #rrggbb in this order:
       black
       red
       green
       brown
       blue
       magenta
       cyan
       white
       bright-black
       bright-red
       bright-green
       bright-brown
       bright-blue
       bright-magenta
       bright-cyan
       bright-white
       default -- can be '-' to use CudaText theme main colors
- 'shell_theme_bg': Terminal background colors. Same format as 'shell_theme_fg'.

- 'terminal_bg_zebra': Difference in "brightness" between adjacent groups
  of terminal tabs of different inactive documents. Accepted values are 
  between 0 (no difference in color) and 50.

- 'wrap': Wrap mode of too long lines.
- 'lock_horizontal_scroll': should terminal view follow printed text horizontally

- 'max_buffer_size': Limits amount of text displayed in the terminal at once (in bytes).
- 'local_history': Max number of commands saved in the terminal's local history. 0 to disable.
- 'global_history': Max number of commands saved in plugin's global history,
  accessed via command 'Search for command in history'.
- 'terminal_height': internal terminal height, for programs that modify their output 
  depending on terminal dimensions (like 'less')


About
-----

Author: Shovel (CudaText forum user, https://github.com/halfbrained/ )
Based on some parts of "Terminal" by Artem Gavrilov & Alexey Torgashin.
License: MIT
