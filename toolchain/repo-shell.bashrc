if [[ -r ~/.bashrc ]]; then
  . ~/.bashrc
fi

if [[ -n ${POLYGLOT_PATH_PREFIX:-} ]]; then
  export PATH="$POLYGLOT_PATH_PREFIX:${PATH:-}"
fi
