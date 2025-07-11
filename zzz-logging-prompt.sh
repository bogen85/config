# vim: set expandtab tabstop=2 shiftwidth=2 filetype=sh :

PS1='\[\e[1;32m\]\u\[\e[1;33m\]@\h\[\e[0m\] \$ '

PS2='> '
PS3='> '
PS4='+ '

[ "root" == $(/usr/bin/whoami) ] && PS1='\[\e[1;31m\]\h\[\e[0m\] \$ '

[ "${inptyfilter}" == "true" ] && PS1="(pty-filter) ${PS1}"

PS1="${VIRTUAL_ENV_PROMPT}${PS1}"

export PS1 PS2 PS3 PS4

#In the future we may want to add more ulimit entries here,
# in the offchance that /etc/security/limits.conf is skipped
#ulimit -Sc 0 #Don't create core files
#ulimit -c unlimited #Do create core files

if test "${TERM}" = "screen"; then
  export WINDOW_IN_PROMPT="\e[1;34m(${WINDOW})"
else
  export ACTUAL_TERM=${TERM}
  export WINDOW_IN_PROMPT=""
fi

TITLE_SUFFIX=""

if [ "$CONTAINER_ID" != "" ] ; then
	TITLE_SUFFIX=" [ @$CONTAINER_ID ]"
fi

if [ "${ACTUAL_TERM}" == "linux" ] ; then
  PROMPT_COMMAND=""
else
  PROMPT_COMMAND='echo -ne "\033]0;${USER}@${HOSTNAME%%.*}:${PWD}${TITLE_SUFFIX}\007" | sed "s:'$HOME':~:"'
fi

### realtime logging

BASHLOG_DIR="${HOME}/.bashlog"
/bin/mkdir -pv ${BASHLOG_DIR}
BASHLOG="${BASHLOG_DIR}/$(date "+%d%b%y.%H:%M:%S@").${PPID}"

HISTTIMEFORMAT="%c: "
PROMPT_COMMAND="history 1 >> ${BASHLOG} ; ${PROMPT_COMMAND}"
PROMPT_COMMAND="stty sane ; ${PROMPT_COMMAND}"
#PROMPT_COMMAND='echo -e "\033[0 q\033[1;36m$(date "+%a %d%b%y %H:%M:%S")\033[1;35m${WINDOW_IN_PROMPT} \033[1;33m${PWD}\033[0m" | sed "s:'$HOME':~:";'"${PROMPT_COMMAND}"
PROMPT_COMMAND='echo -e "\033[1;36m$(date "+%a %d%b%y %H:%M:%S")\033[1;35m${WINDOW_IN_PROMPT} \033[1;33m${PWD}\033[0m" | sed "s:'$HOME':~:";'"${PROMPT_COMMAND}"
export HISTTIMEFORMAT PROMPT_COMMAND

shopt -s checkwinsize
