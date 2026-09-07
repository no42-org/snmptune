#!/bin/sh
# Copyright 2026 Ronny Trommer <ronny@no42.org>
# SPDX-License-Identifier: Apache-2.0
#
# net-snmp "pass" handler for .1.3.6.1.4.1.8072.9999 with three integer
# leaves. Sleeps 100 ms per request to simulate a slow agent region.

base=.1.3.6.1.4.1.8072.9999
sleep 0.1
case "$1" in
  -g)
    case "$2" in
      $base.1.1|$base.1.2|$base.1.3) n=${2##*.}; echo "$2"; echo integer; echo "$((n * 10))" ;;
      *) exit 0 ;;
    esac ;;
  -n)
    case "$2" in
      $base|$base.1) o=$base.1.1 ;;
      $base.1.1) o=$base.1.2 ;;
      $base.1.2) o=$base.1.3 ;;
      *) exit 0 ;;
    esac
    n=${o##*.}; echo "$o"; echo integer; echo "$((n * 10))" ;;
  -s) echo not-writable ;;
esac
