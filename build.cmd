@echo off
rem Builds wiser-dashboard; see tools\build for the options (-all, -target, -package, -matter).
cd /d "%~dp0" && go run ./tools/build %*
