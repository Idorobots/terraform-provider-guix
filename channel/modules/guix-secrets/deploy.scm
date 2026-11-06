;;; SPDX-License-Identifier: MPL-2.0
(define-module (guix-secrets deploy)
  #:use-module (gnu machine)
  #:use-module (gnu machine ssh)
  #:use-module (guix-secrets files)
  #:use-module (ssh session)
  #:use-module (ssh popen)
  #:use-module (ssh channel)
  #:use-module (rnrs io ports)
  #:use-module (rnrs bytevectors)
  #:use-module (ice-9 match)
  #:use-module (ice-9 textual-ports)
  #:export (deploy-files install-command))

(define (install-command destination mode size)
  ;; open-remote-pipe* quotes each argument. Payload bytes travel only on stdin.
  ;; -T prevents mv from interpreting an existing destination as a directory.
  (list "sh" "-c"
        "set -eu; umask 077; d=$1; m=$2; n=$3; p=${d%/*}; [ -n \"$p\" ] || p=/; mkdir -p -- \"$p\"; [ ! -d \"$d\" ]; t=$(mktemp -- \"$p/.guix-secret-XXXXXX\"); trap 'rm -f -- \"$t\"' EXIT HUP INT TERM; cat >\"$t\"; [ \"$(wc -c <\"$t\")\" -eq \"$n\" ]; chown 0:0 -- \"$t\"; chmod \"$m\" -- \"$t\"; mv -fT -- \"$t\" \"$d\""
        "guix-secrets" destination (number->string mode 8) (number->string size)))

(define (deploy-files files machines)
  (validate-payload files)
  (for-each
   (lambda (machine)
     (unless (and (machine? machine)
                  (eq? (machine-environment machine) managed-host-environment-type)
                  (machine-ssh-configuration? (machine-configuration machine)))
       (error "secrets deployment requires managed SSH machines"))
     (let ((session ((@@ (gnu machine ssh) machine-ssh-session) machine)))
       (dynamic-wind
         (const #t)
         (lambda ()
           (for-each
            (match-lambda
              ((destination mode bytes)
               (let ((pipe (apply open-remote-pipe* session OPEN_BOTH
                                  (install-command destination mode (bytevector-length bytes)))))
                 (dynamic-wind
                   (const #t)
                   (lambda ()
                     (put-bytevector pipe bytes)
                     (force-output pipe)
                     (channel-send-eof pipe)
                     ;; Drain output before checking status; never print it.
                     (get-bytevector-all pipe)
                     (unless (zero? (channel-get-exit-status pipe))
                       (throw 'secrets-error "remote secret installation failed; verify the configured SSH user has permission to install root-owned files")))
                   (lambda () (close-port pipe))))))
            files))
         (lambda () (disconnect! session)))))
   machines))
