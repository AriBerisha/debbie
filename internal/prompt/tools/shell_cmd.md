Notes for cmd.exe:
- Put double quotes around arguments that contain spaces or special characters. Single quotes do not quote anything.
- Escape the special characters &, |, <, > and ^ with a caret (^) when you mean them literally.
- Watch out for %VAR% expansion of environment variables.
- Chain commands with && to stop at the first failure; & runs the next command regardless. Don't separate commands with ; or newlines.
- Bash forms such as $'...' quoting and heredocs don't work.
