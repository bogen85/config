Plugin for CudaText.

Advantages over Terminal+ plugin:

- ExTerminal tries to mimic real interactive terminal behaviour.
  That means that you can use apps like nano/vim/htop/python inside it.

To configure, call menu item "Options / Settings-plugins / ExTerminal / Config".
This will open plugins.ini config file, scroll to [exterminal] section.
Options that are boolean must have value 0 or 1.

Commands:
- New terminal
- Close all terminals
- Run selection from editor
- Run current file
- Toggle focus terminal/editor

Options:
- shell_*: shell to execute, for example - "bash" or "cmd.exe".
- colors: display colors in terminal. Disabled by default.
- esc_focuses_editor: focus editor by pressing ESC key instead of sending it to terminal.
    Disabled by default.
- show_caption: show terminal header (title). Disabled by default.
- themed: use theme colors. Disabled by default.
- ctrl_c: set to '1' to enable Ctrl+C (Copy) hotkey in terminal.
- ctrl_x: set to '1' to enable Ctrl+X (Cut) hotkey in terminal.
- ctrl_v: set to '1' to enable Ctrl+V (Paste) hotkey in terminal.

Questions and Answers:
    Q: What if I will shut down the shell by typing `exit` command?
    A: It will be restared.

    Q: I have esc_focuses_editor=1 in my config, how can I send ESC key to terminal app in this case?
    A: Ctrl+[ key sends ESC code to the terminal.

About
-----
Author: veksha (https://github.com/veksha)
License: MIT
