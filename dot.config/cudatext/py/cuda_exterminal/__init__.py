import os
import sys
from cudatext import *
import cudatext_keys as keys
import cudatext_cmd as cmds

from cudax_lib import get_translation,get_opt
_ = get_translation(__file__)  # I18N

MIN_WINDOWS_VER = '10.0.17763'
os_ok = True
if os.name=='nt':
    import platform
    def versiontuple(v):
        return tuple(map(int, (v.split(".")))) if v and '.' in v else (0,0,0)
    version_need = versiontuple(MIN_WINDOWS_VER)    
    version_our = versiontuple(platform.version())

    if version_our < version_need:
        msg_box(_('ExTerminal does not support Windows older than 10'), MB_OK+MB_ICONERROR)
        os_ok = False

if os_ok:
    from .terminal import Terminal
else:
    Terminal = None


TERMINALS_LIMIT = 4

SHELL_UNIX = 'bash'
SHELL_MAC = 'bash'
SHELL_WIN = 'cmd.exe'

IS_WIN = os.name=='nt'
IS_MAC = sys.platform=='darwin'

opt_colors = False
opt_esc_focuses_editor = False
opt_show_caption = False
opt_themed = False
opt_ctrl_c = False
opt_ctrl_x = False
opt_ctrl_v = False

def str_to_bool(s): return s=='1'
def bool_to_str(v): return '1' if v else '0'


fn_icon = os.path.join(os.path.dirname(__file__), 'icons8-console-30.png')
ini = os.path.join(app_path(APP_DIR_SETTINGS), 'plugins.ini')
section = 'exterminal'


class Command:
    def __init__(self):
        self.load_ops()
        self.terminal_id = 0
        self.terminals = []
        self.last_touched = None
        self.h_menu = None

    def load_ops(self):
        self.shell_unix = ini_read(ini, section, 'shell_unix', SHELL_UNIX)
        self.shell_mac = ini_read(ini, section, 'shell_macos', SHELL_MAC)
        self.shell_win = ini_read(ini, section, 'shell_windows', SHELL_WIN)
        if IS_WIN: self.shell_str = self.shell_win
        else: self.shell_str = self.shell_mac if IS_MAC else self.shell_unix

        global opt_colors
        global opt_esc_focuses_editor
        global opt_show_caption
        global opt_themed
        global opt_ctrl_c
        global opt_ctrl_x
        global opt_ctrl_v
        opt_colors   = str_to_bool(ini_read(ini, section, 'colors',   '0'))
        opt_esc_focuses_editor = str_to_bool(ini_read(ini, section, 'esc_focuses_editor', '0'))
        opt_show_caption = str_to_bool(ini_read(ini, section, 'show_caption', '0'))
        opt_themed = str_to_bool(ini_read(ini, section, 'themed', '0'))
        opt_ctrl_c = str_to_bool(ini_read(ini, section, 'ctrl_c', '0'))
        opt_ctrl_x = str_to_bool(ini_read(ini, section, 'ctrl_x', '0'))
        opt_ctrl_v = str_to_bool(ini_read(ini, section, 'ctrl_v', '0'))

    def save_ops(self, only_size=False):
        ini_write(ini, section, 'shell_windows', self.shell_win)
        ini_write(ini, section, 'shell_unix', self.shell_unix)
        ini_write(ini, section, 'shell_macos', self.shell_mac)
        ini_write(ini, section, 'colors',   bool_to_str(opt_colors))
        ini_write(ini, section, 'esc_focuses_editor', bool_to_str(opt_esc_focuses_editor))
        ini_write(ini, section, 'show_caption', bool_to_str(opt_show_caption))
        ini_write(ini, section, 'themed', bool_to_str(opt_themed))
        ini_write(ini, section, 'ctrl_c', bool_to_str(opt_ctrl_c))
        ini_write(ini, section, 'ctrl_x', bool_to_str(opt_ctrl_x))
        ini_write(ini, section, 'ctrl_v', bool_to_str(opt_ctrl_v))

    def config(self):
        self.save_ops()
        file_open(ini)
        lines = [ed.get_text_line(i) for i in range(ed.get_line_count())]
        try:
            index = lines.index('['+section+']')
            ed.set_caret(0, index)
        except:
            pass

    def open(self):
        if not os_ok:
            return
        self.new_terminal_tab()

    def new(self):
        self.new_terminal_tab(focus=True)

    def close_all(self):
        for t in self.terminals:
            t.close()
            del t
        self.terminals = []
        ed.cmd(cmds.cmd_ShowPanelConsole)
        ed.focus()

    def new_terminal_tab(self,focus=False):
        if len(self.terminals) >= TERMINALS_LIMIT:
            msg_box(_("More than {} terminals is not supported yet.").format(TERMINALS_LIMIT), MB_OK+MB_ICONINFO)
            return
        Terminal.themed = opt_themed
        self.set_font_size()

        self.terminal_id += 1
        t = Terminal("ExTerminal {}".format(self.terminal_id),
            self.shell_str,
            opt_esc_focuses_editor,
            fn_icon,
            opt_colors,
            opt_show_caption,
            opt_ctrl_c,
            opt_ctrl_v,
            opt_ctrl_x
            )
        t.form_show_callback = self.form_show_callback
        t.open()
        if focus:
            t.memo.focus()
        self.terminals.append(t)

    def form_show_callback(self, t):
        self.last_touched = t

    def ensure_at_least_one_terminal(self, focus=False):
        # ensure there is at least one terminal
        if len(self.terminals) == 0:
            self.new_terminal_tab(focus=focus)
            # wait for shell
            while self.terminals[0].shell is None:
                app_idle()
                if self.terminals[0].shell == False:
                    # error while executing shell, break from loop
                    break

    def get_active_terminal(self):
        self.ensure_at_least_one_terminal()

        # return visible one
        for t in self.terminals:
            if dlg_proc(t.h_dlg, DLG_PROP_GET)['vis']:
                return t

        # if terminals are hidden
        return self.last_touched

    def show_terminal(self, t):
        app_proc(PROC_BOTTOMPANEL_ACTIVATE, t.name)

    def run_selection(self):
        t = self.get_active_terminal()
        if t:
            self.show_terminal(t)

            txt = ed.get_text_sel()
            if txt:
                for line in txt.split('\n'):
                    t.write(line+'\r')
            else: # if no selection -> run whole line
                caret = ed.get_carets()[0]
                x, y = caret[0:2]
                txt = ed.get_text_line(y).rstrip('\n')
                if txt:
                    t.write(txt+'\r')
                # move caret down
                if y+1 < ed.get_line_count():
                    ed.set_caret(x, y+1)

    def run_current_file(self):
        fn = ed.get_filename()
        if fn:
            t = self.get_active_terminal()
            if t:
                self.show_terminal(t)
                t.write('"'+fn+'"\r')

    def cd_current_file_directory(self):
        fn = ed.get_filename()
        head, tail = os.path.split(fn)
        if fn:
            t = self.get_active_terminal()
            if t:
                self.show_terminal(t)
                t.write('cd "'+head+'"\r')

    def toggle_focus(self):
        if len(self.terminals) == 0:
            self.ensure_at_least_one_terminal(focus=True)
            return

        t = self.get_active_terminal()
        if t is None:
            return
        if not t.memo.get_prop(PROP_FOCUSED):
            self.show_terminal(t)
            t.memo.focus()
        else:
            ed.focus()

    def close_terminal(self, caption):
        for t in self.terminals[:]:
            if t.name == caption:
                t.close()
                self.terminals.remove(t)
                ed.cmd(cmds.cmd_HideBottomPanel)
                return

    def on_sidebar_popup(self, ed_self, caption):
        if 'ExTerminal' in caption:
            if self.h_menu is None:
                self.h_menu = menu_proc(0, MENU_CREATE)
            else:
                menu_proc(self.h_menu, MENU_CLEAR)
            menu_proc(self.h_menu, MENU_ADD, caption=_("New terminal"), command=self.new )
            h_menu_close = menu_proc(self.h_menu, MENU_ADD, caption=_("Close terminal"), command=lambda: self.close_terminal(caption) )
            if not self.terminals:
                menu_proc(h_menu_close, MENU_SET_ENABLED, command=False)
            menu_proc(self.h_menu, MENU_SHOW)

    def on_state(self, ed_self, state):
        if state == APPSTATE_CONFIG_REREAD:
            self.set_font_size()
        elif state == APPSTATE_THEME_UI:
            Terminal.themed = opt_themed
            for t in self.terminals:
                t.set_theme_colors()

    def set_font_size(self):
        _os_suffix = app_proc(PROC_GET_OS_SUFFIX, '')
        Terminal.font_size = get_opt('font_size'+_os_suffix)
