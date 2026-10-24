"""Catalog reachability and native-settings routing, without launching apps."""
import ast
from pathlib import Path
import sys
from types import SimpleNamespace
import unittest
from unittest.mock import patch

BASE=Path(__file__).resolve().parents[1]
sys.path.insert(0,str(BASE))
import application_catalog as catalog
import meadow_settings as settings


class CatalogTests(unittest.TestCase):
    def test_centralized_destinations_all_have_a_real_settings_page(self):
        source=ast.parse((BASE/'meadow_settings.py').read_text())
        page_call=next(n for n in ast.walk(source) if isinstance(n,ast.Call) and n.args
                       and isinstance(n.args[0],ast.Constant) and n.args[0].value=='--page')
        pages=set(ast.literal_eval(next(k.value for k in page_call.keywords if k.arg=='choices')))
        self.assertTrue(set(catalog.SETTINGS_DESTINATIONS.values())<=pages)
        self.assertTrue({tool[6] for tool in catalog.SETTINGS_TOOLS}<=pages)
        self.assertEqual(len(catalog.SETTINGS_TOOLS),len({tool[0] for tool in catalog.SETTINGS_TOOLS}))

    def test_only_known_wrappers_and_settings_are_consolidated_not_daily_tools(self):
        def app(name,categories='Utility;',executable='example'):
            return SimpleNamespace(get_id=lambda:name,get_display_name=lambda:name,
                get_categories=lambda:categories,get_executable=lambda:executable)
        daily=[app('thunar.desktop','System;FileManager;','thunar'),
               app('xfce4-terminal.desktop','System;TerminalEmulator;','xfce4-terminal'),
               app('thunar-bulk-rename.desktop',executable='thunar'),app('unknown-tool.desktop',executable='exo-open')]
        hub=app('meadow-settings.desktop','Settings;')
        moved=[app('xfce4-file-manager.desktop'),app('xfce4-terminal-emulator.desktop'),
               app('xfce4-web-browser.desktop'),app('xfce4-mail-reader.desktop'),
               app('third-party-preferences.desktop','Settings;Utility;')]
        self.assertEqual(catalog.launcher_entries(daily+moved+[hub]),daily+[hub])
        for entry in moved:
            self.assertTrue(catalog.settings_route(entry.get_id(),entry.get_categories()))
        self.assertIsNone(catalog.settings_route('ordinary.desktop','System;NotSettings;'))

    def test_tool_registry_preserves_constant_argv_and_missing_tools_are_absent(self):
        with patch.object(catalog.shutil,'which',side_effect=lambda name:'/a path/terminal' if name=='xfce4-terminal' else None):
            tools=catalog.available_settings_tools()
        self.assertEqual(len(tools),1)
        self.assertEqual(tools[0]['argv'],['/a path/terminal','--preferences'])
        self.assertFalse(tools[0]['embed'])


class SettingsRoutingTests(unittest.TestCase):
    def setUp(self):
        tree=ast.parse((BASE/'meadow_settings.py').read_text())
        cls=next(n for n in ast.walk(tree) if isinstance(n,ast.ClassDef) and n.name=='Settings')
        methods=[n for n in cls.body if isinstance(n,ast.FunctionDef) and n.name in {'open_application','check_panel','panel_removed'}]
        self.calls=[]
        self.tool={'id':'terminal','executable':'/usr/bin/xfce4-terminal','embed':False}
        self.app=SimpleNamespace(launch=lambda args,context:self.calls.append(('launch',args,dict(context.values))))
        class Context:
            def __init__(self):self.values={}
            def setenv(self,key,value):self.values[key]=value
        namespace={'Path':Path,'available_tools':lambda:[self.tool],
            'Gio':SimpleNamespace(DesktopAppInfo=SimpleNamespace(new_from_filename=lambda path:self.app),AppLaunchContext=Context),
            'GLib':SimpleNamespace(Error=RuntimeError,timeout_add=lambda *args:self.calls.append(('timer',args)))}
        exec(compile(ast.Module(body=methods,type_ignores=[]),str(BASE/'meadow_settings.py'),'exec'),namespace)
        self.methods=namespace
        self.owner=SimpleNamespace(open_tool=lambda key:self.calls.append(('tool',key)),
            feedback=SimpleNamespace(set_text=lambda text:self.calls.append(('feedback',text))),
            navigation=SimpleNamespace(select_row=lambda row:self.calls.append(('page',row))),navigation_rows={'general':'general-row'})

    def invoke(self,name,*args):return self.methods[name](self.owner,*args)
    def item(self,name,page,exe='xfce4-terminal'):
        return {'desktop_id':name,'settings_page':page,'executable':exe,'filename':'/installed/real.desktop'}

    def test_regular_terminal_is_launched_as_application_not_preferences(self):
        self.invoke('open_application',self.item('xfce4-terminal.desktop',None))
        self.assertEqual(self.calls[0],('launch',[],{}))
        self.assertFalse(any(call[0]=='tool' for call in self.calls))

    def test_preferences_use_tool_and_hub_alias_selects_existing_page(self):
        self.invoke('open_application',self.item('xfce4-terminal-settings.desktop','applications'))
        self.assertEqual(self.calls,[('tool','terminal')])
        self.calls.clear()
        self.invoke('open_application',self.item('xfce-settings-manager.desktop','general','xfce4-settings-manager'))
        self.assertEqual(self.calls,[('page','general-row')])

    def test_unregistered_settings_launcher_retains_original_desktop_action(self):
        self.invoke('open_application',self.item('vendor-settings.desktop','advanced','vendor-tool'))
        self.assertEqual(self.calls[0],('launch',[],{'MEADOW_UI_ROLE':'settings'}))

    def test_socket_launch_is_fixed_argv_and_invalid_ids_do_not_launch(self):
        tool={'id':'keyboard','argv':['/a path/keyboard'],'embed':True}
        with patch.object(settings,'available_tools',return_value=[tool]),patch.object(settings.subprocess,'Popen') as launch:
            for invalid in (0,-1,'1; echo injected'):
                with self.assertRaises(ValueError):settings.launch_tool('keyboard',invalid)
            launch.assert_not_called()
            settings.launch_tool('keyboard',1234)
            self.assertEqual(launch.call_args.args[0],['/a path/keyboard','--socket-id=1234'])
            self.assertNotIn('shell',launch.call_args.kwargs)
            self.assertEqual(launch.call_args.kwargs['env']['MEADOW_UI_ROLE'],'settings')
            self.assertTrue(launch.call_args.kwargs['start_new_session'])

    def test_stale_panel_callback_cannot_error_or_retry_the_current_panel(self):
        self.owner.alive=True;self.owner.panel_generation=3
        self.owner.panel_process=SimpleNamespace(poll=lambda:None)
        self.owner.panel_socket=SimpleNamespace(get_plug_window=lambda:None)
        self.owner.panel_retry=False
        self.owner.panel_receipt=lambda state:self.calls.append(('receipt',state))
        self.assertFalse(self.invoke('check_panel',2,33))
        self.assertEqual(self.calls,[])
        self.assertFalse(self.owner.panel_retry)
        self.assertFalse(self.invoke('check_panel',3,33))
        self.assertTrue(self.owner.panel_retry)
        self.assertIn(('receipt','timeout'),self.calls)


if __name__=='__main__':unittest.main()
