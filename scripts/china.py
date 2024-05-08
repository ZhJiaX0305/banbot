# -*- coding: utf-8 -*-
import tushare as ts
import datetime
import csv
from typing import *
# tushare的token可从淘宝买
pro = ts.pro_api('20231208200557-1dec8335-58b2-4535-97cd-7d19b7b57f0f')
pro._DataApi__http_url = 'http://tsapi.majors.ltd:7000'


def down_calendars():
    exg_names = ['CFFEX', 'SHFE', 'INE', 'CZCE', 'DCE', 'GFEX']
    start = datetime.date(2010,1,1)
    stop = datetime.datetime.today().date()
    rows = []
    for exg in exg_names:
        print(f'get calendars for {exg}')
        cur_start = start
        dt_list:List[datetime.datetime] = []
        while cur_start < stop:
            cur_stop = cur_start.replace(year=cur_start.year + 5)
            start_ = cur_start.strftime('%Y%m%d')
            stop_ = cur_stop.strftime('%Y%m%d')
            df = pro.query('trade_cal', exchange=exg, start_date=start_, end_date=stop_)
            dates = df[df['is_open'] == 1]['cal_date'].to_list()
            items = []
            for text in dates:
                dt = datetime.datetime.strptime(text, '%Y%m%d')
                items.append(dt.replace(tzinfo=datetime.timezone.utc))
            dt_list.extend(sorted(items))
            cur_start = cur_stop
        dt_ranges = []
        pdate, start_dt = None, None
        for dt in dt_list:
            if pdate is not None:
                exp_date = pdate + datetime.timedelta(days=1)
                if exp_date < dt:
                    dt_ranges.append((start_dt, exp_date))
                    start_dt = dt
            pdate = dt
            if start_dt is None:
                start_dt = dt
        if start_dt is not None:
            dt_ranges.append((start_dt, pdate))
        for a, b in dt_ranges:
            ta = a.strftime('%Y-%m-%d')
            tb = b.strftime('%Y-%m-%d')
            rows.append((exg, ta, tb))
    with open('calendars.csv', 'w', newline='', encoding='utf-8') as file:
        writer = csv.writer(file)
        for row in rows:
            writer.writerow(row)
    print('write complete')

        


if __name__ == '__main__':
    down_calendars()
