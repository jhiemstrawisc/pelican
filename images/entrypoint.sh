#!/bin/bash

supervisord -c /etc/supervisord.conf

if [ "$1" ]; then
  supervisorctl start "pelican_$1"
  Keep the container running
  tail -f /dev/null
else
  echo "A command must be provided"
fi


