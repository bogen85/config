import os
import re
from collections import namedtuple, defaultdict

from cudatext import *
#import cudatext as ct
import cudax_lib as apx
import cudatext_cmd as cmds

# imported on ~access
#from .sansio_lsp_client.structs import MarkupKind
#from .sansio_lsp_client.events import ShowMessage, LogMessage

LogMsg = namedtuple('LogMsg', 'msg type severity')

_   = apx.get_translation(__file__)  # I18N

api_ver = app_api_version()
app_active = True

FORM_W = 550
FORM_H = 350
BUTTON_H = 20
ED_MAX_LINES = 10
FORM_GAP = 4

CURSOR_MOVE_TOLERANCE = 30

def mix_colors(c1, c2):
    r1 = c1 & 0xFF
    g1 = c1 >> 8 & 0xFF
    b1 = c1 >> 16 & 0xFF

    r2 = c2 & 0xFF
    g2 = c2 >> 8 & 0xFF
    b2 = c2 >> 16 & 0xFF

    return (((b1+b2)//2 & 0xff) << 16) | (((g1+g2)//2 & 0xff) << 8) | ((r1+r2)//2 & 0xff);
    
def is_mouse_in_form(h_dlg):
    prop = dlg_proc(h_dlg, DLG_PROP_GET)
    if not prop['vis']: return False
    w = prop['w']
    h = prop['h']

    x, y = app_proc(PROC_GET_MOUSE_POS, '')
    x, y = dlg_proc(h_dlg, DLG_COORD_SCREEN_TO_LOCAL, index=x, index2=y)

    return (0<=x<w and 0<=y<h)

def cursor_dist(pos):
    cursor_pos = app_proc(PROC_GET_MOUSE_POS, '')
    dist_sqr = (pos[0]-cursor_pos[0])**2 + (pos[1]-cursor_pos[1])**2
    return dist_sqr**0.5

# cant invoke method on 'Hint' class
def hint_callback(id_dlg, id_ctl, data='', info=''):
    Hint.on_widget_click(id_ctl, info)


class Hint:
    """ Short-lived dialog with 'Editor', hidden when mouse leaves it
    """
    h = None
    theme_name = None
    current_caret = None
    re_unescape_bslash = None

    @classmethod
    def init_form(cls):
        global MarkupKind
        global FORM_H

        from .sansio_lsp_client.structs import MarkupKind

        _cell_w, cell_h = ed.get_prop(PROP_CELL_SIZE)
        FORM_H = FORM_GAP*2 + ED_MAX_LINES*cell_h + BUTTON_H

        h = dlg_proc(0, DLG_CREATE)

        colors = app_proc(PROC_THEME_UI_DICT_GET, '')
        color_form_bg = colors['TabBorderActive']['color']
        cls.color_btn_font = colors['ButtonFont']['color']
        cls.color_btn_back = colors['ButtonBgPassive']['color']
        cls.color_btn_font_disabled = colors['ButtonFontDisabled']['color']
        cls.color_btn_back_disabled = colors['ButtonBgDisabled']['color']

        dlg_proc(h, DLG_PROP_SET, prop={
                'w': FORM_W + 2*FORM_GAP,
                'border': DBORDER_NONE,
                'color': color_form_bg,
                # doesn't work well with embedded Editor -- using timer hide_check_timer()
                #'on_mouse_exit': cls.dlgcolor_mouse_exit,
                })

        cls._n_sb = dlg_proc(h, DLG_CTL_ADD, 'statusbar')
        dlg_proc(h, DLG_CTL_PROP_SET, index=cls._n_sb, prop={
                'align': ALIGN_BOTTOM,
                'sp_l': 1,
                'sp_r': 1,
                'sp_b': 1,
                'h': BUTTON_H,
                #'w': 128,
                #'w_max': 128,
                })
        cls._h_sb = dlg_proc(h, DLG_CTL_HANDLE, index=cls._n_sb)

        n = dlg_proc(h, DLG_CTL_ADD, 'editor')
        dlg_proc(h, DLG_CTL_PROP_SET, index=n, prop={
                'align': ALIGN_CLIENT,
                'sp_a': FORM_GAP,
                'h': FORM_H,
                'on_click_link': cls.on_click_link,
                })
        h_ed = dlg_proc(h, DLG_CTL_HANDLE, index=n)
        # Editor.set_text_all() doesn't clutter edit history, so no unnecessary stuff is stored in RAM
        edt = Editor(h_ed)

        edt.set_prop(PROP_UNDO_LIMIT, 0)
        edt.set_prop(PROP_GUTTER_ALL, False)
        edt.set_prop(PROP_MINIMAP, False)
        edt.set_prop(PROP_MICROMAP, False)
        edt.set_prop(PROP_LAST_LINE_ON_TOP, False)
        edt.set_prop(PROP_WRAP, WRAP_ON_WINDOW)

        cls.theme_name = app_proc(PROC_THEME_UI_GET, '')

        dlg_proc(h, DLG_SCALE)
        return h, edt
        
    @classmethod
    def set_cursor_margin(cls, cursor_move_tolerance):
        _scale_UI_percent, _scale_font_percent = app_proc(PROC_CONFIG_SCALE_GET, '')
        cls.cursor_margin = cursor_move_tolerance * _scale_UI_percent*0.01 # ~30px scaled

    # language - from deprecated 'MarkedString'
    @classmethod
    def show(cls, text, caret, cursor_loc_start, markupkind=None, language=None, caret_cmds=None):

        #if not ed.get_prop(PROP_FOCUSED):
        #    return # show Hint only if editor is focused (and not autocompletion dialog)

        if not text:
            return

        if cls.h is None  or  cls.is_theme_changed():
            if cls.h is not None: # theme changed
                dlg_proc(cls.h, DLG_FREE)

            cls.h, cls.ed = cls.init_form()

        cls.current_caret = caret # for 'Go to Definition'
        cls.cursor_pos = app_proc(PROC_GET_MOUSE_POS, '')
        cls.tolerance = CURSOR_MOVE_TOLERANCE
        cls.set_cursor_margin(cls.tolerance)

        ### dont show dialog if cursor moved from request-position
        _glob_cursor_start = ed.convert(CONVERT_LOCAL_TO_SCREEN, *cursor_loc_start)
        if cursor_dist(_glob_cursor_start) > cls.cursor_margin:
            return

        ### dialog Editor setup
        cls.ed.set_prop(PROP_RO, False)
        try:
            if markupkind == MarkupKind.MARKDOWN:
                cls.ed.set_prop(PROP_LEXER_FILE, 'Markdown')
                import html
                text = html.unescape(text)
                text = cls.unescape_bslash(text)
            else:
                cls.ed.set_prop(PROP_LEXER_FILE, None)

            cls.ed.set_text_all(text)
            cls.ed.set_prop(PROP_LINE_TOP, 0)
            cls.ed.set_prop(PROP_SCROLL_HORZ, 0)
        finally:
            cls.ed.set_prop(PROP_RO, True)

        ### calculate dialog position and dimensions: x,y, h,w
        l,t,r,b = ed.get_prop(PROP_RECT_TEXT)
        cell_w, cell_h = ed.get_prop(PROP_CELL_SIZE)
        ed_size_x = r - l # text area sizes - to not obscure other ed-controls

        caret_loc_px = ed.convert(CONVERT_CARET_TO_PIXELS, x=caret[0], y=caret[1])
        if caret_loc_px is None: return
        top_hint = caret_loc_px[1]-t > b-caret_loc_px[1] # space up is larger than down
        y0,y1 = (t, caret_loc_px[1])  if top_hint else  (caret_loc_px[1], b)
        space_h = y1-y0 - FORM_GAP*2
        if not top_hint:
            space_h -= cell_h
        _scale_UI_percent, _scale_font_percent = app_proc(PROC_CONFIG_SCALE_GET, '')
        h = min(FORM_H * _scale_UI_percent // 100, space_h)
        w = min(FORM_W * _scale_UI_percent // 100, ed_size_x)

        x = caret_loc_px[0] - int(w*0.5) # center over caret
        if x < l: # dont fit on left
            x = l + FORM_GAP
        elif x+w > r: # dont fit on right
            x = r - w - FORM_GAP

        if top_hint:
            y = (caret_loc_px[1] - (h + FORM_GAP))
        else:
            y = (caret_loc_px[1] + cell_h + FORM_GAP)


        dlg_proc(cls.h, DLG_PROP_SET, prop={
                'p': ed.get_prop(PROP_HANDLE_SELF ), #set parent to Editor handle
                'x': x,
                'y': y,
                'w': w,
                'h': h,
                })

        cls.caret_cmds = caret_cmds
        if caret_cmds:
            cls.fill_cmds(caret_cmds, min(FORM_W, ed_size_x))

        # first - large delay, after - smaller
        timer_proc(TIMER_START_ONE, Hint.hide_check_timer, 750, tag='initial')
        dlg_proc(cls.h, DLG_SHOW_NONMODAL)

    @classmethod
    def fill_cmds(cls, cmds, width):
        statusbar_proc(cls._h_sb, STATUSBAR_DELETE_ALL)

        cellwidth = int(width/len(cmds)) + 1
        callback_fstr = 'module=cuda_lsp.dlg;func=hint_callback;info="{}";'
        for caption,cmd in cmds.items():
            cellind = statusbar_proc(cls._h_sb, STATUSBAR_ADD_CELL, index=-1)
            statusbar_proc(cls._h_sb, STATUSBAR_SET_CELL_TEXT, index=cellind, value=caption)
            statusbar_proc(cls._h_sb, STATUSBAR_SET_CELL_SIZE, index=cellind, value=cellwidth)

            if cmd:
                bg,fg = cls.color_btn_back,  cls.color_btn_font

                callback = callback_fstr.format(caption)
                statusbar_proc(cls._h_sb, STATUSBAR_SET_CELL_CALLBACK, index=cellind, value=callback)
            else:
                bg,fg = cls.color_btn_back_disabled,  cls.color_btn_font_disabled

            statusbar_proc(cls._h_sb,  STATUSBAR_SET_CELL_COLOR_BACK, index=cellind, value=bg)
            statusbar_proc(cls._h_sb,  STATUSBAR_SET_CELL_COLOR_FONT, index=cellind, value=fg)


    @classmethod
    def on_widget_click(cls, n, info):
        if n == cls._n_sb:
            f = cls.caret_cmds.get(info)
            if f:
                f(caret=cls.current_caret)

    @classmethod
    def on_click_link(cls, id_dlg, id_ctl, data='', info=''):
        if data:
            apx.safe_open_url(data)

    @classmethod
    def set_max_lines(cls, nlines):
        global ED_MAX_LINES

        ED_MAX_LINES = nlines

    @classmethod
    def hide_check_timer(cls, tag='', info=''):
        global app_active
        
        # hide if not over dialog  and  cursor moved at least ~15px
        left_button_down = 'L' in app_proc(PROC_GET_KEYSTATE, '')
        
        # give the user possibility to return cursor to Hint window
        # decrease by 50 every time to a minimum of CURSOR_MOVE_TOLERANCE value
        cls.tolerance = max(CURSOR_MOVE_TOLERANCE, cls.tolerance-50)
        if left_button_down:
            cls.tolerance = 500
        # reset when value gets to 300
        if cls.tolerance == 300:
            cls.tolerance = CURSOR_MOVE_TOLERANCE # 30
        # update cursor_margin
        if cls.cursor_margin != cls.tolerance:
            cls.set_cursor_margin(cls.tolerance)
        
        if not cls.is_visible(): #stop the timer if dialog was already closed (could be closed by autocompletion)
            timer_proc(TIMER_STOP, Hint.hide_check_timer, 250, tag='')
        elif app_active \
             and not is_mouse_in_form(cls.h) \
             and cursor_dist(cls.cursor_pos) > cls.cursor_margin \
             and not left_button_down:
            timer_proc(TIMER_STOP, Hint.hide_check_timer, 250, tag='')

            cls.hide()
            ed.focus()
            #print('ed.focus in hide_check_timer()')
        else:
            cls.cursor_pos = app_proc(PROC_GET_MOUSE_POS, '')                

        if tag == 'initial': # give some time to move mouse to dialog
            timer_proc(TIMER_START, Hint.hide_check_timer, 250, tag='')

    @classmethod
    def hide(cls):
        if not cls.h:    return
        # clear editor data and hide dialog
        cls.ed.set_prop(PROP_RO, False)
        cls.ed.set_text_all('')
        cls.current_caret = None
        dlg_proc(cls.h, DLG_HIDE)

    @classmethod
    def is_theme_changed(cls):
        old_name = cls.theme_name
        cls.theme_name = app_proc(PROC_THEME_UI_GET, '')
        return old_name != cls.theme_name

    @classmethod
    def is_visible(cls):
        if cls.h is None:
            return False
        return dlg_proc(cls.h, DLG_PROP_GET)['vis']

    @classmethod
    def is_under_cursor(cls):
        return cls.is_visible()  and  is_mouse_in_form(cls.h)

    @classmethod
    def unescape_bslash(cls, text):
        if cls.re_unescape_bslash is None:
            import re

            cls.re_unescape_bslash = re.compile(r'\\([^\\])')

        return cls.re_unescape_bslash.sub(r'\1', text)


SPL = chr(1)
_icons_dir = os.path.join(os.path.dirname(os.path.realpath(__file__)), 'icons')
PANEL_LOG_TAG = app_proc(PROC_GET_UNIQUE_TAG, '') # jic

TYPE_MSG = 'type_msgs'
TYPE_LOG = 'type_logs'
TYPE_DIAG = 'type_diags'

SEVERITY_ERR = 'svr_err'
SEVERITY_WRN = 'svr_wrn'
SEVERITY_INFO = 'svr_inf'
SEVERITY_LOG = 'svr_log'
SEVERITY_NA = 'svr_na'

SEVERITYS = [
    SEVERITY_ERR,
    SEVERITY_WRN,
    SEVERITY_INFO,
    SEVERITY_LOG,
]
SEVERITY_MAP = {
    1: SEVERITY_ERR,
    2: SEVERITY_WRN,
    3: SEVERITY_INFO,
    4: SEVERITY_LOG,
}
SEVERITY_IC_PATHS = {
    SEVERITY_ERR:  os.path.join(_icons_dir, 'error.png'),
    SEVERITY_WRN:  os.path.join(_icons_dir, 'warning.png'),
    SEVERITY_INFO: os.path.join(_icons_dir, 'information.png'),
    SEVERITY_LOG:  os.path.join(_icons_dir, 'hint.png'),
    SEVERITY_NA:   os.path.join(_icons_dir, 'severity_na.png'),
}

PANEL_CAPTIONS = {
    TYPE_MSG:       _('Messages'),
    TYPE_LOG:       _('Logs'),
    TYPE_DIAG:      _('Diagnostics'),

    SEVERITY_ERR:   _('Error'),
    SEVERITY_WRN:   _('Warning'),
    SEVERITY_INFO:  _('Info'),
    SEVERITY_LOG:   _('Log'),
}

# PanelLog proxy
def on_panellog_sb_click(id_dlg, id_ctl, data='', info=''):
    PanelLog.on_sb_click(id_dlg, id_ctl, data, info)


class PanelLog:

    fn_icon = os.path.join(os.path.dirname(__file__), 'icons', 'lsp.png')

    panels = {} # name to instance

    _colors = None

    TAG_ED_MENU_WRAP = 'ed_wrap'

    def __init__(self, panel_name, state=None):
        global LogMessage, ShowMessage
        from .sansio_lsp_client import LogMessage, ShowMessage

        PanelLog.panels[panel_name] = self
        PanelLog.type_captions = {
            ShowMessage: TYPE_MSG,
            LogMessage: TYPE_LOG,
        }

        self.name = panel_name

        self._msgs = [] # ShowMessage, LogMessage, tuple(type_, str)
        self._extra_types = set() # server stderr, etc
        # filter panel: disabled "categories"
        self._disabled_items = set(state.get('log_panel_filter'))  if isinstance(state, dict) else  set()
        self._is_wrap = state.get('is_wrap')  if isinstance(state, dict) else  True

        self._memo_pos = (0,0)
        self._severity_ims = {} # severity str -> icon ind in imagelist
        self._have_na_severity = False
        self._sb_cellind_map = {} # name -> cellind
        self._h_btn_sidebar = None
        self._h_ed_menu = None

        self._init_panel()
        self._setup_decor_gutter()

        # use severities imagelist from memo
        h_im = self._memo.decor(DECOR_GET_IMAGELIST)
        statusbar_proc(self._h_sb, STATUSBAR_SET_IMAGELIST, value=h_im)

        self._reset_memo()
        self._update_sb()

    @property
    def sidepanel_name(self):
        return 'LSP: ' + str(self.name)

    @property
    def colors(self):
        if self._colors is None:
            self._colors = app_proc(PROC_THEME_UI_DICT_GET, '')
        return self._colors

    def memo_on_click_dbl(self, id_dlg, id_ctl, data='', info=''):
        y = self._memo.get_carets()[0][1]
        lines = self._memo.get_text_all().splitlines()

        fn = ''
        for i in reversed(range(y)):
            line = lines[i]
            if line.startswith('File: '):
                fn = line[6:]
                break
        if not fn: return

        text = self._memo.get_text_line(y)
        if not text.startswith('Line '): return
        n = text.find(': ')
        if n<0: return
        line = int(text[5:n])-1
        file_open(fn)
        ed.set_caret(0, line)
        ed.focus()
        
    def on_show(self, *args, **vargs):
        timer_proc(TIMER_START_ONE, self._scroll_to_end, 50, tag='')

    def _init_panel(self):
        self.h_dlg = dlg_proc(0, DLG_CREATE)
        dlg_proc(self.h_dlg, DLG_PROP_SET, prop={ 'on_show': self.on_show })

        # Memo ##########
        n = dlg_proc(self.h_dlg, DLG_CTL_ADD, prop='editor')
        dlg_proc(self.h_dlg, DLG_CTL_PROP_SET, index=n, prop={
            'name': 'memo',
            'align': ALIGN_CLIENT,
            'on_menu': self.on_ed_menu,
            })
        h_memo = dlg_proc(self.h_dlg, DLG_CTL_HANDLE, index=n)
        self._memo = Editor(h_memo)
        dlg_proc(self.h_dlg, DLG_CTL_PROP_SET, index=n, prop={
            'on_click_dbl': self.memo_on_click_dbl,
            } )

        # Top buttons #######
        n = dlg_proc(self.h_dlg, DLG_CTL_ADD, prop='statusbar')
        dlg_proc(self.h_dlg, DLG_CTL_PROP_SET, index=n, prop={
            'name': 'statusbar',
            'align': ALIGN_TOP,
            })
        self._h_sb = dlg_proc(self.h_dlg, DLG_CTL_HANDLE, index=n)

        self._set_memo_wrap(self._is_wrap)

        self._memo.set_prop(PROP_GUTTER_ALL,    True)
        self._memo.set_prop(PROP_GUTTER_BM,     True)
        self._memo.set_prop(PROP_GUTTER_FOLD,   False)
        self._memo.set_prop(PROP_GUTTER_NUM,    False)
        self._memo.set_prop(PROP_GUTTER_STATES, False)

        self._memo.set_prop(PROP_UNDO_LIMIT,        0)
        self._memo.set_prop(PROP_MINIMAP,           False)
        self._memo.set_prop(PROP_MICROMAP,          False)
        self._memo.set_prop(PROP_LAST_LINE_ON_TOP,  False)
        self._memo.set_prop(PROP_HILITE_CUR_LINE,   False)

        dlg_proc(self.h_dlg, DLG_SCALE)

        app_proc(PROC_BOTTOMPANEL_ADD_DIALOG, (self.sidepanel_name,  self.h_dlg,  self.fn_icon))
        #app_proc(PROC_SIDEPANEL_ADD_DIALOG, (self.sidepanel_name,  self.h_dlg,  self.fn_icon))

        for props in app_proc(PROC_BOTTOMPANEL_ENUM_ALL, ''):
            if props['cap'] == self.sidepanel_name:
                self._h_btn_sidebar = props['btn_h']
                break

    def _setup_decor_gutter(self):
        for severity_str, icon_path  in SEVERITY_IC_PATHS.items():
            _h_im = self._memo.decor(DECOR_GET_IMAGELIST)
            _ind = imagelist_proc(_h_im, IMAGELIST_ADD, value=icon_path)
            self._severity_ims[severity_str] = _ind

    def _get_ed_menu(self):
        """ copy, select all, clear, toggle wrap,
        """
        if self._h_ed_menu is None:
            import cudatext_cmd

            self._h_ed_menu = menu_proc(0, MENU_CREATE)
            h_menu = self._h_ed_menu

            _la_copy =      lambda *args,**vargs: self._memo.cmd(cudatext_cmd.cCommand_ClipboardCopy)
            _la_select_all = lambda *args,**vargs: self._memo.cmd(cudatext_cmd.cCommand_SelectAll)
            _la_wrap  =     lambda *args,**vargs: self.toggle_wrap()
            _la_clear =     lambda *args,**vargs: self.clear()

            menu_proc(h_menu, MENU_ADD, command=_la_copy,       caption=_('Copy'))
            menu_proc(h_menu, MENU_ADD, command=_la_select_all, caption=_('Select all'))
            menu_proc(h_menu, MENU_ADD,   caption='-')
            menu_proc(h_menu, MENU_ADD, command=_la_wrap, caption=_('Toggle word wrap'),
                                                                        tag=self.TAG_ED_MENU_WRAP)
            menu_proc(h_menu, MENU_ADD,   caption='-')
            menu_proc(h_menu, MENU_ADD, command=_la_clear, caption=_('Clear'))
        #end if
        return self._h_ed_menu


    def _update_sb(self):
        """ update filters bar - to reflect current state
        """
        h_sb = self._h_sb

        bg_color = self.colors['TabActive']['color']

        font_enabled_color = self.colors['TabFontActive']['color']
        font_disabled_color = self.colors['TabFontMod']['color']

        line_enabled_color = 0x7cc87c #7cc87c

        self._sb_cellind_map.clear()

        # clear
        statusbar_proc(h_sb, STATUSBAR_DELETE_ALL)

        statusbar_proc(h_sb, STATUSBAR_SET_COLOR_BORDER_R, value=self.colors['TabBorderActive']['color'])

        callbac_fstr = 'module=cuda_lsp.dlg;func=on_panellog_sb_click;info="{}";'
        ###### FILL
        # Left: classes (Msg, Log, Other)  +  State - On/Off
        for name in [TYPE_MSG,  TYPE_LOG,  *sorted(self._extra_types)]:
            cellind = statusbar_proc(h_sb, STATUSBAR_ADD_CELL, index=-1)
            self._sb_cellind_map[name] = cellind

            _caption = PANEL_CAPTIONS.get(name, name)
            statusbar_proc(h_sb, STATUSBAR_SET_CELL_TEXT, index=cellind, value=_caption)
            _callback = callbac_fstr.format(name)
            statusbar_proc(h_sb, STATUSBAR_SET_CELL_CALLBACK, index=cellind, value=_callback)

            _font_col = font_disabled_color
            #if name not in self._disabled_types: # if enabled
            if name not in self._disabled_items: # if enabled
                _font_col = font_enabled_color
                statusbar_proc(h_sb, STATUSBAR_SET_CELL_COLOR_LINE2, index=cellind,
                                                                        value=line_enabled_color)
            statusbar_proc(h_sb, STATUSBAR_SET_CELL_AUTOSIZE, index=cellind, value=True)
            statusbar_proc(h_sb, STATUSBAR_SET_CELL_COLOR_BACK, index=cellind, value=bg_color)
            statusbar_proc(h_sb, STATUSBAR_SET_CELL_COLOR_FONT, index=cellind, value=_font_col)


        # add spacer
        cellind = statusbar_proc(h_sb, STATUSBAR_ADD_CELL, index=-1)
        statusbar_proc(h_sb, STATUSBAR_SET_CELL_AUTOSTRETCH, index=cellind, value=True)
        statusbar_proc(h_sb, STATUSBAR_SET_CELL_COLOR_BACK, index=cellind, value=bg_color)

        # add severity filters  +  State - On/Off
        for name in SEVERITYS:
            cellind = statusbar_proc(h_sb, STATUSBAR_ADD_CELL, index=-1)
            self._sb_cellind_map[name] = cellind

            _im_ind = self._severity_ims[name]
            statusbar_proc(h_sb, STATUSBAR_SET_CELL_IMAGEINDEX, index=cellind, value=_im_ind)
            _callback = callbac_fstr.format(name)
            statusbar_proc(h_sb, STATUSBAR_SET_CELL_CALLBACK, index=cellind, value=_callback)
            _hint = PANEL_CAPTIONS.get(name, 'NA')
            statusbar_proc(h_sb, STATUSBAR_SET_CELL_HINT, index=cellind, value=_hint)
            statusbar_proc(h_sb, STATUSBAR_SET_CELL_ALIGN, index=cellind, value='C')

            #_font_col = font_disabled_color
            if name not in self._disabled_items: # if enabled
                #_font_col = font_enabled_color
                statusbar_proc(h_sb, STATUSBAR_SET_CELL_COLOR_LINE2, index=cellind,
                                                                        value=line_enabled_color)
            statusbar_proc(h_sb, STATUSBAR_SET_CELL_AUTOSIZE, index=cellind, value=True)
            statusbar_proc(h_sb, STATUSBAR_SET_CELL_COLOR_BACK, index=cellind, value=bg_color)
            #statusbar_proc(h_sb, STATUSBAR_SET_CELL_COLOR_FONT, index=cellind, value=_font_col)

        self._update_counts()

    def _update_counts(self, *args, **vargs):
        msg_counts = self._get_msg_counts()

        for name,cellind in self._sb_cellind_map.items():
            _overlay = msg_counts.get(name, '')
            statusbar_proc(self._h_sb, STATUSBAR_SET_CELL_OVERLAY, index=cellind, value=str(_overlay))

    def _update_memo(self):
        self._reset_memo()
        self._memo.decor(DECOR_DELETE_BY_TAG, tag=PANEL_LOG_TAG)

        self._append_memo_msgs([msg for msg in self._msgs if self._filter_msg(msg)])
        
        ## some bugs workarounds:
        # make sure ..line_top is never greater then ..line_count
        if self._memo.get_prop(PROP_LINE_TOP) >= self._memo.get_line_count()-1:
            self._memo.set_prop(PROP_LINE_TOP, self._memo.get_line_count()-1)
        else:
            # repaint bug workaround:
            # this will repaint scrollbar/editor (after removing a lot of lines it can be needed)
            self._memo.set_prop(PROP_LINE_TOP, self._memo.get_prop(PROP_LINE_TOP))
        ##

    def log(self, msg):   # events: ShowMessage, LogMessage
        severity_str = SEVERITY_MAP[msg.type.value]
        self.log_str(msg.message, type_=type(msg), severity=severity_str, scroll=True)

    def log_str(self, s, type_, severity=SEVERITY_NA, scroll=False, update_memo=True):
        if s and s[-1] != '\n':
            s += '\n'

        lm = LogMsg(s, type=type_, severity=severity)

        self._msgs.append(lm)
        update_memo and self._update_memo()

        timer_proc(TIMER_START_ONE, self._update_counts, 500)
        self._update_sidebar()

        # add na severity if needed
        if severity == SEVERITY_NA  and  not self._have_na_severity:
            SEVERITYS.append(SEVERITY_NA)
            self._have_na_severity = True
        # new log type appeared
        if isinstance(type_, str)  and  type_ not in self._extra_types:
            self._extra_types.add(type_)
            self._update_sb()

    def clear_diagnostics(self):
        self._msgs = [ msg for msg in self._msgs if PanelLog.type_captions.get(msg.type, msg.type) != TYPE_DIAG ]
        self._update_memo()
        self._update_counts()
        self._update_sidebar()
        
    def clear(self):
        self._msgs.clear()
        self._update_memo()
        self._update_counts()
        self._update_sidebar()

    def toggle_wrap(self):
        self._is_wrap = not self._is_wrap
        self._set_memo_wrap(self._is_wrap)

    def set_lex(self, lexer):
        self._memo.set_prop(PROP_LEXER_FILE, lexer)


    def _update_sidebar(self):
        if self._h_btn_sidebar:
            overlay_text = str(len(self._msgs)) if len(self._msgs) > 0 else '' # clear if zero
            button_proc(self._h_btn_sidebar, BTN_SET_OVERLAY, overlay_text)
            
    def _scroll_to_end(self, *args, **vargs):
        self._memo.cmd(cmds.cCommand_GotoTextEnd)

    def _append_memo_msgs(self, msgs, scroll=False):
        if not msgs:
            return
        # calculate line positions (for decor)
        positions = []
        for msg in msgs:
            line_cnt = len(msg.msg.strip().split('\n')) # message can be multiline
            positions.append(self._memo_pos[1]) # append current position
            self._memo_pos = (0, self._memo_pos[1] + line_cnt) # update insertion point

        # insert all messages in one go (fast!)
        newpos = self._memo.insert(*self._memo_pos, ''.join([msg.msg for msg in msgs]))
        
        # decor
        for n,msg in enumerate(msgs):
            _nline = positions[n]
            _imind = self._severity_ims[msg.severity]
            self._memo.decor(DECOR_SET, line=_nline, image=_imind, tag=PANEL_LOG_TAG)
        
    def _append_memo_msg(self, msg, scroll=False):
        _nline = self._memo_pos[1]
        newpos = self._memo.insert(*self._memo_pos, msg.msg)
        scroll and self._scroll_to_end()

        if newpos is not None:
            self._memo_pos = newpos
            #### decor icon
            _imind = self._severity_ims[msg.severity]
            self._memo.decor(DECOR_SET, line=_nline, image=_imind, tag=PANEL_LOG_TAG)
        else:
            print(f'NOTE: LSP: failed to show msg: {type_, len(txt), txt[:64]}')

    def _reset_memo(self):
        self._memo.set_text_all('')
        self._memo_pos = (0,0)

    def _set_memo_wrap(self, is_wrap):
        _wrap = WRAP_ON_WINDOW  if is_wrap else  WRAP_OFF
        self._memo.set_prop(PROP_WRAP, _wrap)


    def _filter_msg(self, msg):
        type_str = PanelLog.type_captions.get(msg.type, msg.type)

        if type_str not in self._disabled_items  and  msg.severity not in self._disabled_items:
            return True

    def _get_msg_counts(self):
        r = defaultdict(int)
        for msg in self._msgs:
            type_str = PanelLog.type_captions.get(msg.type, msg.type)
            r[type_str] += 1
            r[msg.severity] += 1
        return r


    def get_state(self):
        state = {
            'log_panel_filter': list(self._disabled_items),
            'is_wrap': self._is_wrap,
        }
        return state


    def on_ed_menu(self, id_dlg, id_ctl, data='', info=''):
        # (139819628679408, 0, {'btn': 1, 'state': '', 'x': 248, 'y': 115}, '')
        h_menu = self._get_ed_menu()
        for item in menu_proc(h_menu, MENU_ENUM):
            if item.get('tag') == self.TAG_ED_MENU_WRAP:
                menu_proc(item['id'], MENU_SET_CHECKED, command=self._is_wrap)
                break
        menu_proc(h_menu, MENU_SHOW)
        return False


    def close(self):
        self._reset_memo()
        self._msgs.clear()

        app_proc(PROC_BOTTOMPANEL_REMOVE, self.sidepanel_name)
        dlg_proc(self.h_dlg, DLG_FREE)

        del PanelLog.panels[self.name]


    @classmethod
    def on_sb_click(cls, id_dlg, id_ctl, data='', info=''):
        """ send event to the clicked panel (by .h_dlg), and refresh
        """
        for plog in cls.panels.values():
            if plog.h_dlg == id_dlg:
                if info in plog._disabled_items:
                    plog._disabled_items.remove(info)
                else:
                    plog._disabled_items.add(info)

                plog._update_sb()
                plog._update_memo()
                break

    @classmethod
    def on_theme_change(cls):
        """ update colored stuff for every panel
        """
        import cudatext as ct

        colors = app_proc(PROC_THEME_UI_DICT_GET, '')

        for plog in cls.panels.values():
            plog._colors = colors   # reset saved colors
            plog._update_sb()

            # memo colors
            for name,val in vars(ct).items():
                if name.startswith('COLOR_ID_') and type(val) == str:
                    theme_item_name = val
                    theme_item = colors.get(theme_item_name)
                    if theme_item is not None:
                        theme_col = theme_item['color']
                        plog._memo.set_prop(PROP_COLOR, (theme_item_name, theme_col))


    @classmethod
    def get_logger(cls, panel_name, state):
        """ Main way to create panel objects
        """
        if panel_name not in cls.panels:
            cls.panels[panel_name] = PanelLog(panel_name, state)

        return cls.panels[panel_name]

class SignaturesDialog:
    
    themed = True
    color_font = 0
    color_bg = apx.html_color_to_int('ffffe1')
    color_dimmed = apx.html_color_to_int('909090')
    color_hilite = apx.html_color_to_int('0000FF')
    h = None
    memo: Editor = None
    spacing = 2
    param_pos = 0
    dim_unfocused_value = 0
    wrap_info_loaded = False
    
    @classmethod
    def move_window(cls):
        if cls.h and cls.memo:
            mon_x1, mon_y1, mon_x2, mon_y2 = app_proc(PROC_COORD_MONITOR,0)
            screen_w, screen_h = (mon_x2 - mon_x1, mon_y2 - mon_y1)

            dlg_proc(cls.h, DLG_CTL_PROP_SET, name='memo', prop={
                'w': screen_w, 'h': screen_h
            })

            # tooltip must have the same font_size / scale as editor
            font_prop = ed.get_prop(PROP_FONT, '')
            scale = ed.get_prop(PROP_SCALE_FONT, '')
            cls.memo.set_prop(PROP_FONT, font_prop)
            cls.memo.set_prop(PROP_SCALE_FONT, scale)

            wrapped_lines = 0
            while True:
                app_idle()
                if cls.wrap_info_loaded:
                    wrap_info = cls.memo.get_wrapinfo()
                    wrapped_lines = max(1, len(wrap_info))
                    break 

            #max_line_len = 10
            # for first painting of tooltip [Linux qt5]
            max_line_len = cls.memo.get_line_len(0)
            for line in wrap_info:
                max_line_len = max(max_line_len, line['len'])

            cell_x, cell_y = cls.memo.get_prop(PROP_CELL_SIZE, 0)
            ed_cell_x, ed_cell_y = ed.get_prop(PROP_CELL_SIZE, 0)
            cell_x = max(cell_x, 7) # for first painting of tooltip [Linux qt5]
            cell_y = max(cell_y, 16)
            h = wrapped_lines * cell_y + (cls.spacing*4)
            w = max_line_len * cell_x + (cls.spacing*4)
            
            caret_x, caret_y = ed.get_carets()[0][:2]
            
            # caret x/y to screen x/y
            x, y = ed.convert(CONVERT_CARET_TO_PIXELS, caret_x, caret_y) or (0, 0)
            x, y = ed.convert(CONVERT_LOCAL_TO_SCREEN, x, y) or (0, 0)
            
            # offset x position in hope that parameter in tooltip will be close to caret pos
            #if cls.param_pos:
                #x = x - cls.param_pos*cell_x
            markers = cls.memo.attr(MARKERS_GET)
            for marker in markers:
                if marker[0] == 1:
                    xy = cls.memo.convert(CONVERT_CARET_TO_PIXELS, marker[1]+1, marker[2])
                    if xy:
                        param_pos, _ = xy
                        param_len = marker[3]
                        x = x - (param_pos+(param_len*cell_x//2)) + cell_x
                        break
            
            # do not allow to move behind screen edges
            _y = y-cell_y*wrapped_lines-cls.spacing*6
            if _y >= 0:     y = _y
            else:           y = y+ed_cell_y+cls.spacing*2

            if x + w > mon_x1+screen_w:
                x = mon_x1+screen_w - w
            if x < mon_x1:   x = mon_x1
            if y < mon_y1:   y = mon_y1
            
            dlg_proc(cls.h, DLG_PROP_SET, prop={
                'x': x,
                'y': y,
                'w': w,
                'h': h,
                'color': cls.color_bg,
                })
    
    @classmethod
    def set_text(cls, signatures):
        if cls.h is None:
            cls.h, cls.memo = cls.init_form()
            
        if cls.themed:
            colors = app_proc(PROC_THEME_UI_DICT_GET, '')
            cls.color_font = colors['ListFont']['color']
            cls.color_bg = colors['ListBg']['color']
            cls.color_hilite = colors['ListFontHotkey']['color']
            cls.color_dimmed = mix_colors(cls.color_font, cls.color_bg)

        cls.memo.set_prop(PROP_COLOR, (COLOR_ID_TextFont, cls.color_font))
        cls.memo.set_prop(PROP_COLOR, (COLOR_ID_TextBg, cls.color_bg))

        signatures, activeSignature, activeParameter = signatures
        if activeSignature is None or activeSignature >= len(signatures):
            activeSignature = 0
        
        cls.param_pos = 0
        cls.memo.set_prop(PROP_RO, False)
        cls.memo.set_text_all('')
        cls.memo.dim(DIM_DELETE_ALL)
        
        from .sansio_lsp_client.structs import SignatureInformation
        sig: SignatureInformation
        for i,sig in enumerate(signatures):
            cls.memo.set_text_line(-2, sig.label)
            if i != activeSignature:
                #cls.memo.attr(MARKERS_ADD, x=0, y=i, len=len(sig.label), color_font=cls.color_dimmed)
                cls.memo.dim(DIM_ADD, i, i, 150)
            
            if not sig.parameters:  continue
            
            if activeParameter is None or activeParameter >= len(sig.parameters):
                activeParameter = 0
            
            activeParameter = sig.activeParameter if sig.activeParameter is not None else activeParameter
            
            param = sig.parameters[activeParameter].label
            if isinstance(param, tuple):
                x1, x2 = param
                cls.memo.attr(MARKERS_ADD, x=x1, y=i, len=x2-x1, color_font=cls.color_hilite, tag=1)
                cls.param_pos = x1
            elif isinstance(param, str):
                # replace comma with special char (excluding ones inside [{()}] brackets)
                brackets = 0
                signature = sig.label
                signature = re.sub(".*?\(","", signature, count=1) # remove method name and first bracket '('
                params_start = len(sig.label) - len(signature) # remember position where parameters start
                signature = re.sub("\)[^,)]*$","", signature, count=1) # remove last bracket ')' and return value
                char_list = list(signature)
                for j,c in enumerate(char_list):
                    if 0:pass
                    elif c in '[{(':    brackets += 1
                    elif c in ']})':    brackets -= 1
                    elif c == ',':
                        if brackets <= 0:   char_list[j]=chr(1)
                
                parts = re.split(r'\x01', ''.join(char_list))
                pos = params_start
                skipping = True
                skipped = -1
                for j,part in enumerate(parts):
                    param_name = re.split(':|=',part)[0]
                    param_name = param_name.strip().replace('*','')
                    first_real_param = sig.parameters[0].label.strip().replace('*','')
                    first_real_param = re.split(':|=',first_real_param)[0]
                    if skipping and param_name != first_real_param:
                        skipped += 1
                        pos += len(part)+1
                        continue # skip 'self' or 'cls', etc...
                    else:
                        skipping = False
                    if j-skipped == activeParameter+1:
                        cls.memo.attr(MARKERS_ADD, x=pos, y=i, len=len(part), color_font=cls.color_hilite, tag=1)
                        cls.param_pos = pos
                        break
                    pos += len(part)+1
        cls.memo.set_prop(PROP_LINE_TOP, 0)
        cls.memo.set_prop(PROP_SCROLL_HORZ, 0)
        cls.memo.set_prop(PROP_RO, True)

    @classmethod
    def show(cls):
        cls.wrap_info_loaded = False
        if cls.h is None:
            cls.h, cls.memo = cls.init_form()
            
        if cls.is_visible():
            timer_proc(TIMER_STOP, cls.hide, 8000, tag='')
            timer_proc(TIMER_START_ONE, cls.hide, 8000, tag='')
            cls.wrap_info_loaded = True
            cls.move_window()
            return
        
        dlg_proc(cls.h, DLG_PROP_SET, prop={ 'taskbar': 2 })
        
        # workaround for `dim_unfocused` option
        # (save value to variable, set to 0, then restore from variable after ed.focus())
        if api_ver >= '1.0.429':
            cls.dim_unfocused_value = ed.get_prop(PROP_DIM_UNFOCUSED)
            #print('setting from ',cls.dim_unfocused_value, 'to 0')
            ed.set_prop(PROP_DIM_UNFOCUSED, 0)
        
        
        dlg_proc(cls.h, DLG_SHOW_NONMODAL)
        dlg_proc(cls.h, DLG_PROP_SET, prop={
                    'w': 0, 'h': 0 })
        
        # ed.focus() will be called inside timer (workaround for Linux)
        timer_proc(TIMER_START_ONE, cls.unfocus, 50, tag='')

        timer_proc(TIMER_STOP, cls.hide, 8000, tag='')
        timer_proc(TIMER_START_ONE, cls.hide, 8000, tag='')
        
        cls.move_window()
        
    @classmethod
    def init_form(cls):
        h=dlg_proc(0, DLG_CREATE)
        dlg_proc(h, DLG_PROP_SET, prop={
            'cap': 'Tooltip',
            'topmost': True,
            'border': DBORDER_NONE,
            'taskbar': 2,
        })
        cls.h = h
        
        _, font_size = ed.get_prop(PROP_FONT)
        font_scale = ed.get_prop(PROP_SCALE_FONT)
        
        idc=dlg_proc(h, DLG_CTL_ADD, 'editor');
        dlg_proc(cls.h, DLG_CTL_PROP_SET, index=idc, prop={
            'border': DBORDER_NONE,
            'name': 'memo',
            #'align': ALIGN_CLIENT,
            'font_size': font_size,
            'sp_a': cls.spacing
        })
        cls.memo = Editor(dlg_proc(h, DLG_CTL_HANDLE, index=idc))
        cls.memo.set_prop(PROP_SCALE_FONT, font_scale)
        cls.memo.set_prop(PROP_UNDO_LIMIT, 0)
        cls.memo.set_prop(PROP_LAST_LINE_ON_TOP, False)
        cls.memo.set_prop(PROP_GUTTER_NUM, False)
        cls.memo.set_prop(PROP_GUTTER_STATES, False)
        cls.memo.set_prop(PROP_GUTTER_FOLD, False)
        cls.memo.set_prop(PROP_GUTTER_BM, False)
        cls.memo.set_prop(PROP_MINIMAP, False)
        cls.memo.set_prop(PROP_MICROMAP, False)
        cls.memo.set_prop(PROP_SCROLLSTYLE_VERT, SCROLLSTYLE_HIDE)
        cls.memo.set_prop(PROP_SCROLLSTYLE_HORZ, SCROLLSTYLE_HIDE)
        cls.memo.set_prop(PROP_CARET_VIEW, (0, 0, False))
        cls.memo.set_prop(PROP_CARET_VIEW_RO, cls.memo.get_prop(PROP_CARET_VIEW))
        cls.memo.set_prop(PROP_WRAP, WRAP_ON_WINDOW)
        cls.memo.set_prop(PROP_HILITE_CUR_LINE, False)
        cls.memo.set_prop(PROP_THEMED, False)
        
        dlg_proc(h, DLG_SCALE)
        return h, cls.memo

    @classmethod
    def unfocus(cls, tag='', info=''):
        global app_active
        cls.wrap_info_loaded = True
        if app_active:
            ed.focus()
            #print('ed.focus in unfocus()')
        if api_ver >= '1.0.429':
            #print('restoring to',cls.dim_unfocused_value)
            ed.set_prop(PROP_DIM_UNFOCUSED, cls.dim_unfocused_value)
        
    @classmethod
    def hide(cls, tag='', info=''):
        if not cls.h:    return
        dlg_proc(cls.h, DLG_HIDE)

    @classmethod
    def is_visible(cls):
        if cls.h is None:
            return False
        return dlg_proc(cls.h, DLG_PROP_GET)['vis']

    @classmethod
    def on_theme_change(cls):
        cls.hide()
