Plugin for CudaText.
Gives intelligence commands for Python lexer.

*  Auto-completion.
   To use it: place caret after incomplete function/class/variable/module name,
   and call CudaText command "auto-completion menu" (Ctrl+Space).

*  Go to definition.
   To use it: place caret on a name of function/class/variable/module, and call
   CudaText command "go to definition" (or use menu item "Go to definition"
   in the editor context menu, or use mouse shortcut).

*  Show function call-tip.
   To use it: place caret after function name between () brackets, and call
   CudaText command "show function-hint" (Ctrl+Shift+Space).
   For example, enter such script, caret is shown as "|":
     import os
     fn = os.path.join(|)
   Command will show the parameters of "os.path.join" in the floating panel
   at the top of CudaText window.

*  Plugin command "Show function doc-string".
   Shows doc-string for function/class name under caret, in the Output panel.
   (To call Output panel, click on the lower part of CudaText sidebar.)

*  Plugin command "Show usages".
   Shows menu with locations (file name, line number) where identifier under caret
   is used. After choosing the item in menu, editor jumps to chosen location.


Refactoring commands, they change editor text:

*  Refactoring - Rename
   Renames all instances of identifier under the caret.
*  Refactoring - Inline
   Inlines a variable under the caret. This is basically the opposite
   of extracting a variable.
*  Refactoring - Extract variable
   Moves an expression to a new statemenet.
*  Refactoring - Extract function
   Moves an expression to a new function.

Plugin handles CudaText projects (internally calling Project Manager plugin).

Based on Jedi library:
  Jedi is a static analysis tool for Python that can be used in IDEs/editors.
  https://github.com/davidhalter/jedi

Authors:
  Alexey Torgashin (CudaText)
  Oleh Lutsak https://github.com/OlehL/
License: MIT
