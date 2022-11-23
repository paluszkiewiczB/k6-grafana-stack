import http from 'k6/http';
import {check, sleep} from 'k6';

export default function () {
    const res = http.get('http://localhost:7080/unstable');
    check(res, {
        'status is 200': (r) => r.status === 200
    });
    sleep(1);
}