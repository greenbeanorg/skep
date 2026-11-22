#!/bin/sh
# M7c validation on hearst: the probe cases of ADR 0011 Decision 12 item 2,
# run against a jail whose port filter the agent installed. Test tooling.
#
# Env: RUN (log name), JA/JV (attacker and victim jail names), PA/PV (their
# host ports), BR (bridge), MODE (static|dhcp), AMAC/VMAC, AIP/VIP,
# V1WANT (0 or +), CASES (list of case names; default all of MODE's).
set -u
set -f   # filters contain [ ]: no globbing
D=/tmp/m7cv/$RUN
mkdir -p "$D"
FORGED=02:5e:00:00:00:01
npass=0
nfail=0

cap() { # name iface filter
	sudo timeout 9 tcpdump -n -e -l -i "$2" $3 > "$D/$1.$2" 2>/dev/null &
}

# t NAME FILTER WANT command... : WANT 0 = nothing matching FILTER may reach
# the bridge or the victim's port; + = something must reach the bridge.
t() {
	name=$1; filt=$2; want=$3; shift 3
	echo "==== $MODE $name $(date -u +%H:%M:%S)"
	cap "$name" "$PA" ""
	cap "$name" "$BR" "$filt"
	cap "$name" "$PV" "$filt"
	sleep 1
	for c in "$@"; do
		echo "  \$ $c"
		sh -c "$c" 2>&1 | sed 's/^/     /'
	done
	wait
	npre=$(grep -c . "$D/$name.$PA")
	nbr=$(grep -c . "$D/$name.$BR")
	nv=$(grep -c . "$D/$name.$PV")
	echo "  -- sent on $PA (all frames): $npre; on $BR matching [$filt]: $nbr; on $PV matching: $nv"
	sed 's/^/     br: /' "$D/$name.$BR" | head -4
	if [ "$want" = 0 ] && [ "$nbr" -eq 0 ] && [ "$nv" -eq 0 ]; then v=PASS
	elif [ "$want" = + ] && [ "$nbr" -gt 0 ]; then v=PASS
	else v=FAIL; fi
	[ $v = PASS ] && npass=$((npass+1)) || nfail=$((nfail+1))
	echo "VERDICT $MODE $name $v (want $want, bridge $nbr, victim $nv, sent $npre)"
}

hex_ip() { # 192.0.2.50 -> 0xc0000232
	echo "$1" | awk -F. '{ printf "0x%02x%02x%02x%02x\n", $1, $2, $3, $4 }'
}

A="sudo jexec $JA"
L2="$A /root/rawl2 -if ${L2IF:-skep0}"
H50=$(hex_ip 192.0.2.50); H60=$(hex_ip 192.0.2.60); HV=$(hex_ip "$VIP")
if [ "$MODE" = static ]; then S=0; SE=0; else S=+; SE=+; fi   # dhcp and vm are MAC-only   # S: what MAC-only lets through

want_case() { [ -z "${CASES:-}" ] && return 0; case " $CASES " in *" $1 "*) return 0;; esac; return 1; }

want_case D1 && t D1 "icmp and ether src $AMAC" + "$A ping -c 2 -t 3 $VIP"
want_case D2 && t D2 "icmp and ether src $VMAC" + "sudo jexec $JV ping -c 2 -t 3 $AIP"
want_case A1 && t A1 "ether src $FORGED" 0 "$L2 -mode icmp -eth-src $FORGED -eth-dst $VMAC -src $AIP -dst $VIP -count 2"
want_case A2 && t A2 "ether src $FORGED" 0 "$A ifconfig skep0 ether $FORGED" "$A ping -c 2 -t 3 $VIP" "$A ifconfig skep0 ether $AMAC"
want_case B1 && t B1 "ip src 192.0.2.50" $S "$A /root/rawping -src 192.0.2.50 -dst $VIP -count 2"
want_case B2 && t B2 "ip src 198.51.100.10" $S "$A /root/rawping -src 198.51.100.10 -dst $VIP -count 2"
want_case C1 && t C1 "arp and ether src $AMAC and arp[14:4] = $H50" $S "$L2 -mode arp -op reply -eth-src $AMAC -eth-dst $VMAC -sha $AMAC -spa 192.0.2.50 -tha $VMAC -tpa $VIP -count 2"
want_case C2 && t C2 "arp and arp[8:4] = 0x025e0000 and arp[12:2] = 0x0001" 0 "$L2 -mode arp -op reply -eth-src $AMAC -eth-dst $VMAC -sha $FORGED -spa $AIP -tha $VMAC -tpa $VIP -count 2" "sudo jexec $JV arp -an"
want_case C3 && t C3 "ether src $AMAC and (ip src 192.0.2.50 or (arp and arp[14:4] = $H50))" $S "$A ifconfig skep0 inet 192.0.2.50/32 alias" "$A ping -c 2 -t 3 -S 192.0.2.50 $VIP" "$A ifconfig skep0 inet 192.0.2.50 -alias"
want_case C4 && t C4 "ether src $AMAC and arp and arp[14:4] = $HV" $S "$L2 -mode arp -op request -eth-src $AMAC -sha $AMAC -spa $VIP -tpa $VIP -count 2"
want_case C5 && t C5 "ether src $AMAC and arp and arp[14:4] = $H50" 0 "$A arp -s 192.0.2.50 $FORGED pub" "sudo jexec $JV ping -c 1 -t 2 192.0.2.50" "$A arp -d 192.0.2.50" "sudo jexec $JV arp -an"
want_case T5 && t T5 "ether src $AMAC and (ip src 192.0.2.60 or (arp and arp[14:4] = $H60))" $S "$A ifconfig skep0 inet 192.0.2.60/24 alias" "$A ping -c 2 -t 3 -S 192.0.2.60 $VIP" "$A ifconfig skep0 inet 192.0.2.60 -alias"
want_case E1 && t E1 "ether src $AMAC and udp dst port 67" $SE "$L2 -mode dhcp -eth-src $AMAC -count 2"
want_case E2 && t E2 "ether src $FORGED" 0 "$L2 -mode dhcp -eth-src $FORGED -count 2"
want_case E3 && t E3 "arp and arp[8:4] = 0x025e0000 and arp[12:2] = 0x0001" 0 "$L2 -mode arp -op request -eth-src $AMAC -sha $FORGED -spa $AIP -tpa $VIP -count 2"
want_case F1 && t F1 "ip6 and ether src $AMAC" ${F1WANT:-0} "$L2 -mode ipv6 -eth-src $AMAC -count 2"
want_case G1 && t G1 "udp src port 67" 0 "$L2 -mode dhcpoffer -eth-src $AMAC -src $AIP -count 2"
want_case G2 && t G2 "icmp6 and ether src $AMAC and ip6[40] = 134" 0 "$L2 -mode ra -eth-src $AMAC -count 2"
want_case L1 && t L1 "ether src $AMAC and ether proto 0x88cc" 0 "$L2 -mode lldp -eth-src $AMAC -count 2"
want_case L2 && t L2 "ether src $AMAC and stp" 0 "$L2 -mode stp -eth-src $AMAC -count 2"
want_case V1 && t V1 "vlan 5" "${V1WANT:-+}" "$L2 -mode icmp -vlan 5 -eth-src $AMAC -eth-dst $VMAC -src $AIP -dst $VIP -count 2"
want_case V2 && t V2 "ether src $FORGED" 0 "$L2 -mode icmp -vlan 5 -eth-src $FORGED -eth-dst $VMAC -src $AIP -dst $VIP -count 2"
want_case V3 && t V3 "vlan" 0 "$L2 -mode icmp -vlan 5,7 -eth-src $AMAC -eth-dst $VMAC -src $AIP -dst $VIP -count 2"
echo "SUMMARY $RUN $MODE: $npass passed, $nfail failed"
